package autonat

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p-core/crypto"
	cryptopb "github.com/libp2p/go-libp2p-core/crypto/pb"
	"github.com/libp2p/go-libp2p-core/helpers"
	"github.com/libp2p/go-libp2p-core/host"
	"github.com/libp2p/go-libp2p-core/network"
	"github.com/libp2p/go-libp2p-core/peer"

	pb "github.com/libp2p/go-libp2p-autonat/pb"

	ggio "github.com/gogo/protobuf/io"
	autonat "github.com/libp2p/go-libp2p-autonat"
	ma "github.com/multiformats/go-multiaddr"
	manet "github.com/multiformats/go-multiaddr-net"
	"github.com/multiformats/go-varint"
)

const P_CIRCUIT = 290

var (
	AutoNATServiceDialTimeout   = 15 * time.Second
	AutoNATServiceResetInterval = 1 * time.Minute

	AutoNATServiceThrottle = 3
)

// AutoNATService provides NAT autodetection services to other peers
type AutoNATService struct {
	ctx    context.Context
	dialer host.Host

	// rate limiter
	mx   sync.Mutex
	reqs map[peer.ID]int

	// for the identity certificate
	cpk *cryptopb.PublicKey
	sk  crypto.PrivKey
}

// NewAutoNATService creates a new AutoNATService instance attached to a host
func NewAutoNATService(ctx context.Context, h host.Host, opts ...libp2p.Option) (*AutoNATService, error) {
	opts = append(opts, libp2p.NoListenAddrs)
	dialer, err := libp2p.New(ctx, opts...)
	if err != nil {
		return nil, err
	}

	as := &AutoNATService{
		ctx:    ctx,
		dialer: dialer,
		reqs:   make(map[peer.ID]int),
	}
	h.SetStreamHandler(autonat.AutoNATProto, as.handleStream)

	// keys for the identity certificate
	pk := dialer.Peerstore().PubKey(dialer.ID())
	sk := dialer.Peerstore().PrivKey(dialer.ID())
	cpk, err := crypto.PublicKeyToProto(pk)
	if err != nil {
		return nil, fmt.Errorf("failed to transform public key to proto,err=%s", err)
	}
	as.cpk = cpk
	as.sk = sk

	go as.resetRateLimiter()

	return as, nil
}

func (as *AutoNATService) handleStream(s network.Stream) {
	defer helpers.FullClose(s)

	pid := s.Conn().RemotePeer()
	log.Debugf("New stream from %s", pid.Pretty())

	r := ggio.NewDelimitedReader(s, network.MessageSizeMax)
	w := ggio.NewDelimitedWriter(s)

	var req pb.Message
	var res pb.Message

	err := r.ReadMsg(&req)
	if err != nil {
		log.Debugf("Error reading message from %s: %s", pid.Pretty(), err.Error())
		s.Reset()
		return
	}

	t := req.GetType()
	if t != pb.Message_DIAL {
		log.Debugf("Unexpected message from %s: %s (%d)", pid.Pretty(), t.String(), t)
		s.Reset()
		return
	}

	dr := as.handleDial(pid, s.Conn().RemoteMultiaddr(), req.GetDial().GetPeer())
	res.Type = pb.Message_DIAL_RESPONSE.Enum()
	res.DialResponse = dr

	// add an identity certificate
	dr.DialerIdentityCertificate = new(pb.Message_DialerIdentityCertificate)
	dr.DialerIdentityCertificate.PublicKey = as.cpk
	sgn, err := as.sk.Sign(varint.ToUvarint(*req.Dial.Nonce))
	if err != nil {
		log.Infof("failed to sign nonce %d sent by client, err=%s", *req.Dial.Nonce, err)
		s.Reset()
		return
	}
	dr.DialerIdentityCertificate.Signature = sgn

	err = w.WriteMsg(&res)
	if err != nil {
		log.Debugf("Error writing response to %s: %s", pid.Pretty(), err.Error())
		s.Reset()
		return
	}
}

func (as *AutoNATService) handleDial(p peer.ID, obsaddr ma.Multiaddr, mpi *pb.Message_PeerInfo) *pb.Message_DialResponse {
	if mpi == nil {
		return newDialResponseError(pb.Message_E_BAD_REQUEST, "missing peer info")
	}

	mpid := mpi.GetId()
	if mpid != nil {
		mp, err := peer.IDFromBytes(mpid)
		if err != nil {
			return newDialResponseError(pb.Message_E_BAD_REQUEST, "bad peer id")
		}

		if mp != p {
			return newDialResponseError(pb.Message_E_BAD_REQUEST, "peer id mismatch")
		}
	}

	addrs := make([]ma.Multiaddr, 0)
	seen := make(map[string]struct{})

	// add observed addr to the list of addresses to dial
	if !as.skipDial(obsaddr) {
		addrs = append(addrs, obsaddr)
		seen[obsaddr.String()] = struct{}{}
	}

	for _, maddr := range mpi.GetAddrs() {
		addr, err := ma.NewMultiaddrBytes(maddr)
		if err != nil {
			log.Debugf("Error parsing multiaddr: %s", err.Error())
			continue
		}

		if as.skipDial(addr) {
			continue
		}

		str := addr.String()
		_, ok := seen[str]
		if ok {
			continue
		}

		addrs = append(addrs, addr)
		seen[str] = struct{}{}
	}

	if len(addrs) == 0 {
		return newDialResponseError(pb.Message_E_DIAL_ERROR, "no dialable addresses")
	}

	return as.doDial(peer.AddrInfo{ID: p, Addrs: addrs})
}

func (as *AutoNATService) skipDial(addr ma.Multiaddr) bool {
	// skip relay addresses
	_, err := addr.ValueForProtocol(P_CIRCUIT)
	if err == nil {
		return true
	}

	// skip private network (unroutable) addresses
	if !manet.IsPublicAddr(addr) {
		return true
	}

	return false
}

func (as *AutoNATService) doDial(pi peer.AddrInfo) *pb.Message_DialResponse {
	// rate limit check
	as.mx.Lock()
	count := as.reqs[pi.ID]
	if count >= AutoNATServiceThrottle {
		as.mx.Unlock()
		return newDialResponseError(pb.Message_E_DIAL_REFUSED, "too many dials")
	}
	as.reqs[pi.ID] = count + 1
	as.mx.Unlock()

	ctx, cancel := context.WithTimeout(as.ctx, AutoNATServiceDialTimeout)
	defer cancel()

	err := as.dialer.Connect(ctx, pi)
	if err != nil {
		log.Debugf("error dialing %s: %s", pi.ID.Pretty(), err.Error())
		// wait for the context to timeout to avoid leaking timing information
		// this renders the service ineffective as a port scanner
		<-ctx.Done()
		return newDialResponseError(pb.Message_E_DIAL_ERROR, "dial failed")
	}

	conns := as.dialer.Network().ConnsToPeer(pi.ID)
	if len(conns) == 0 {
		log.Errorf("supposedly connected to %s, but no connection to peer", pi.ID.Pretty())
		return newDialResponseError(pb.Message_E_INTERNAL_ERROR, "internal service error")
	}

	ra := conns[0].RemoteMultiaddr()
	as.dialer.Network().ClosePeer(pi.ID)
	return newDialResponseOK(ra)
}

func (as *AutoNATService) resetRateLimiter() {
	ticker := time.NewTicker(AutoNATServiceResetInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			as.mx.Lock()
			as.reqs = make(map[peer.ID]int)
			as.mx.Unlock()

		case <-as.ctx.Done():
			return
		}
	}
}
