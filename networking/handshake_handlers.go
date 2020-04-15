// (c) 2019-2020, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package networking

// #include "salticidae/network.h"
// bool checkPeerCertificate(msgnetwork_conn_t *, bool, void *);
// void unknownPeerHandler(netaddr_t *, x509_t *, void *);
// void peerHandler(peernetwork_conn_t *, bool, void *);
// void ping(msg_t *, msgnetwork_conn_t *, void *);
// void pong(msg_t *, msgnetwork_conn_t *, void *);
// void getVersion(msg_t *, msgnetwork_conn_t *, void *);
// void version(msg_t *, msgnetwork_conn_t *, void *);
// void getPeerList(msg_t *, msgnetwork_conn_t *, void *);
// void peerList(msg_t *, msgnetwork_conn_t *, void *);
import "C"

import (
	"errors"
	"fmt"
	"math"
	"time"
	"unsafe"

	"github.com/sasha-s/go-deadlock"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ava-labs/salticidae-go"

	"github.com/ava-labs/gecko/ids"
	"github.com/ava-labs/gecko/snow/networking"
	"github.com/ava-labs/gecko/snow/validators"
	"github.com/ava-labs/gecko/utils"
	"github.com/ava-labs/gecko/utils/hashing"
	"github.com/ava-labs/gecko/utils/logging"
	"github.com/ava-labs/gecko/utils/random"
	"github.com/ava-labs/gecko/utils/timer"
)

/*
Receive a new connection.
 - Send version message.
Receive version message.
 - Validate data
 - Send peer list
 - Mark this node as being connected
*/

/*
Periodically gossip peerlists.
 - Only connected stakers should be gossiped.
 - Gossip to a caped number of peers.
 - The peers to gossip to should be at least half full of stakers (or all the
	stakers should be in the set).
*/

/*
Attempt reconnections
 - If a non-staker disconnects, delete the connection
 - If a staker disconnects, attempt to reconnect to the node for awhile. If the
    node isn't connected to after awhile delete the connection.
*/

const (
	// CurrentVersion this avalanche instance is executing.
	CurrentVersion = "avalanche/0.0.1"
	// MaxClockDifference allowed between connected nodes.
	MaxClockDifference = time.Minute
	// PeerListGossipSpacing is the amount of time to wait between pushing this
	// node's peer list to other nodes.
	PeerListGossipSpacing = time.Minute
	// PeerListGossipSize is the number of peers to gossip each period.
	PeerListGossipSize = 100
	// PeerListStakerGossipFraction calculates the fraction of stakers that are
	// gossiped to. If set to 1, then only stakers will be gossiped to.
	PeerListStakerGossipFraction = 2
	// GetVersionTimeout is the amount of time to wait before sending a
	// getVersion message to a partially connected peer
	GetVersionTimeout = 2 * time.Second
	// ReconnectTimeout is the amount of time to wait to reconnect to a staker
	// before giving up
	ReconnectTimeout = 1 * time.Minute
)

// Manager is the struct that will be accessed on event calls
var (
	HandshakeNet = Handshake{}
)

var (
	errDSValidators = errors.New("couldn't get validator set of default subnet")
)

// Handshake handles the authentication of new peers. Only valid stakers
// will appear connected.
type Handshake struct {
	handshakeMetrics

	networkID uint32

	log           logging.Logger
	vdrs          validators.Set
	myAddr        salticidae.NetAddr
	myID          ids.ShortID
	net           salticidae.PeerNetwork
	enableStaking bool // Should only be false for local tests

	clock       timer.Clock
	pending     AddrCert // Connections that I haven't gotten version messages from
	connections AddrCert // Connections that I think are connected

	versionTimeout   timer.TimeoutManager
	reconnectTimeout timer.TimeoutManager
	peerListGossiper *timer.Repeater

	awaitingLock deadlock.Mutex
	awaiting     []*networking.AwaitingConnections
}

// Initialize to the c networking library. This should only be done once during
// node setup.
func (nm *Handshake) Initialize(
	log logging.Logger,
	vdrs validators.Set,
	myAddr salticidae.NetAddr,
	myID ids.ShortID,
	peerNet salticidae.PeerNetwork,
	registerer prometheus.Registerer,
	enableStaking bool,
	networkID uint32,
) {
	log.AssertTrue(nm.net == nil, "Should only register network handlers once")
	nm.log = log
	nm.vdrs = vdrs
	nm.myAddr = myAddr
	nm.myID = myID
	nm.net = peerNet
	nm.enableStaking = enableStaking
	nm.networkID = networkID

	net := peerNet.AsMsgNetwork()

	net.RegConnHandler(salticidae.MsgNetworkConnCallback(C.checkPeerCertificate), nil)
	peerNet.RegPeerHandler(salticidae.PeerNetworkPeerCallback(C.peerHandler), nil)
	peerNet.RegUnknownPeerHandler(salticidae.PeerNetworkUnknownPeerCallback(C.unknownPeerHandler), nil)
	net.RegHandler(Ping, salticidae.MsgNetworkMsgCallback(C.ping), nil)
	net.RegHandler(Pong, salticidae.MsgNetworkMsgCallback(C.pong), nil)
	net.RegHandler(GetVersion, salticidae.MsgNetworkMsgCallback(C.getVersion), nil)
	net.RegHandler(Version, salticidae.MsgNetworkMsgCallback(C.version), nil)
	net.RegHandler(GetPeerList, salticidae.MsgNetworkMsgCallback(C.getPeerList), nil)
	net.RegHandler(PeerList, salticidae.MsgNetworkMsgCallback(C.peerList), nil)

	nm.handshakeMetrics.Initialize(nm.log, registerer)

	nm.versionTimeout.Initialize(GetVersionTimeout)
	go nm.log.RecoverAndPanic(nm.versionTimeout.Dispatch)

	nm.reconnectTimeout.Initialize(ReconnectTimeout)
	go nm.log.RecoverAndPanic(nm.reconnectTimeout.Dispatch)

	nm.peerListGossiper = timer.NewRepeater(nm.gossipPeerList, PeerListGossipSpacing)
	go nm.log.RecoverAndPanic(nm.peerListGossiper.Dispatch)
}

// AwaitConnections ...
func (nm *Handshake) AwaitConnections(awaiting *networking.AwaitingConnections) {
	nm.awaitingLock.Lock()
	defer nm.awaitingLock.Unlock()

	awaiting.Add(nm.myID)
	for _, cert := range nm.connections.IDs().List() {
		awaiting.Add(cert)
	}
	if awaiting.Ready() {
		go awaiting.Finish()
	} else {
		nm.awaiting = append(nm.awaiting, awaiting)
	}
}

func (nm *Handshake) gossipPeerList() {
	stakers := []ids.ShortID{}
	nonStakers := []ids.ShortID{}
	for _, id := range nm.connections.IDs().List() {
		if nm.vdrs.Contains(id) {
			stakers = append(stakers, id)
		} else {
			nonStakers = append(nonStakers, id)
		}
	}

	numStakersToSend := (PeerListGossipSize + PeerListStakerGossipFraction - 1) / PeerListStakerGossipFraction
	if len(stakers) < numStakersToSend {
		numStakersToSend = len(stakers)
	}
	numNonStakersToSend := PeerListGossipSize - numStakersToSend
	if len(nonStakers) < numNonStakersToSend {
		numNonStakersToSend = len(nonStakers)
	}

	idsToSend := []ids.ShortID{}
	sampler := random.Uniform{N: len(stakers)}
	for i := 0; i < numStakersToSend; i++ {
		idsToSend = append(idsToSend, stakers[sampler.Sample()])
	}
	sampler.N = len(nonStakers)
	sampler.Replace()
	for i := 0; i < numNonStakersToSend; i++ {
		idsToSend = append(idsToSend, nonStakers[sampler.Sample()])
	}

	ips := []salticidae.NetAddr{}
	for _, id := range idsToSend {
		if ip, exists := nm.connections.GetIP(id); exists {
			ips = append(ips, ip)
		}
	}

	nm.SendPeerList(ips...)
}

// Connections returns the object that tracks the nodes that are currently
// connected to this node.
func (nm *Handshake) Connections() Connections { return &nm.connections }

// Shutdown the network
func (nm *Handshake) Shutdown() {
	nm.versionTimeout.Stop()
	nm.peerListGossiper.Stop()
}

// SendGetVersion to the requested peer
func (nm *Handshake) SendGetVersion(addr salticidae.NetAddr) {
	build := Builder{}
	gv, err := build.GetVersion()
	nm.log.AssertNoError(err)
	nm.send(gv, addr)

	nm.numGetVersionSent.Inc()
}

// SendVersion to the requested peer
func (nm *Handshake) SendVersion(addr salticidae.NetAddr) error {
	build := Builder{}
	v, err := build.Version(nm.networkID, nm.clock.Unix(), CurrentVersion)
	if err != nil {
		return fmt.Errorf("packing Version failed due to %s", err)
	}
	nm.send(v, addr)
	nm.numVersionSent.Inc()
	return nil
}

// SendPeerList to the requested peer
func (nm *Handshake) SendPeerList(addrs ...salticidae.NetAddr) error {
	if len(addrs) == 0 {
		return nil
	}

	ips, ids := nm.connections.Conns()
	ipsToSend := []utils.IPDesc(nil)
	for i, id := range ids {
		if nm.vdrs.Contains(id) {
			ipsToSend = append(ipsToSend, ips[i])
		}
	}

	if len(ipsToSend) == 0 {
		nm.log.Debug("No IPs to send to %d peer(s)", len(addrs))
		return nil
	}

	nm.log.Verbo("Sending %d ips to %d peer(s)", len(ipsToSend), len(addrs))

	build := Builder{}
	pl, err := build.PeerList(ipsToSend)
	if err != nil {
		return fmt.Errorf("Packing Peerlist failed due to %w", err)
	}
	nm.send(pl, addrs...)
	nm.numPeerlistSent.Add(float64(len(addrs)))
	return nil
}

func (nm *Handshake) send(msg Msg, addrs ...salticidae.NetAddr) {
	ds := msg.DataStream()
	defer ds.Free()
	ba := salticidae.NewByteArrayMovedFromDataStream(ds, false)
	defer ba.Free()
	cMsg := salticidae.NewMsgMovedFromByteArray(msg.Op(), ba, false)
	defer cMsg.Free()

	switch len(addrs) {
	case 0:
	case 1:
		nm.net.SendMsg(cMsg, addrs[0])
	default:
		nm.net.MulticastMsgByMove(cMsg, addrs)
	}
}

// checkPeerCertificate of a new inbound connection
//export checkPeerCertificate
func checkPeerCertificate(_ *C.struct_msgnetwork_conn_t, connected C.bool, _ unsafe.Pointer) C.bool {
	return connected
}

func (nm *Handshake) connectedToPeer(conn *C.struct_peernetwork_conn_t, addr salticidae.NetAddr) {
	ip := toIPDesc(addr)
	// If we're enforcing staking, use a peer's certificate to uniquely identify them
	// Otherwise, use a hash of their ip to identify them
	cert := ids.ShortID{}
	ipCert := toShortID(ip)
	if nm.enableStaking {
		cert = getPeerCert(conn)
	} else {
		cert = ipCert
	}

	nm.log.Debug("Connected to %s", ip)

	longCert := cert.LongID()
	nm.reconnectTimeout.Remove(longCert)
	nm.reconnectTimeout.Remove(ipCert.LongID())

	nm.pending.Add(addr, cert)

	handler := new(func())
	*handler = func() {
		if nm.pending.ContainsIP(addr) {
			nm.SendGetVersion(addr)
			nm.versionTimeout.Put(longCert, *handler)
		}
	}
	(*handler)()
}

func (nm *Handshake) disconnectedFromPeer(addr salticidae.NetAddr) {
	cert := ids.ShortID{}
	if pendingCert, exists := nm.pending.GetID(addr); exists {
		cert = pendingCert
	} else if connectedCert, exists := nm.connections.GetID(addr); exists {
		cert = connectedCert
	} else {
		return
	}

	nm.log.Info("Disconnected from %s", toIPDesc(addr))

	longCert := cert.LongID()
	if nm.vdrs.Contains(cert) {
		nm.reconnectTimeout.Put(longCert, func() {
			nm.net.DelPeer(addr)
		})
	} else {
		nm.net.DelPeer(addr)
	}
	nm.versionTimeout.Remove(longCert)

	if !nm.enableStaking {
		nm.vdrs.Remove(cert)
	}

	nm.pending.RemoveIP(addr)
	nm.connections.RemoveIP(addr)
	nm.numPeers.Set(float64(nm.connections.Len()))

	nm.awaitingLock.Lock()
	defer nm.awaitingLock.Unlock()
	for _, awaiting := range HandshakeNet.awaiting {
		awaiting.Remove(cert)
	}
}

// peerHandler notifies a change to the set of connected peers
// connected is true if a new peer is connected
// connected is false if a formerly connected peer has disconnected
//export peerHandler
func peerHandler(_conn *C.struct_peernetwork_conn_t, connected C.bool, _ unsafe.Pointer) {
	pConn := salticidae.PeerNetworkConnFromC(salticidae.CPeerNetworkConn(_conn))
	addr := pConn.GetPeerAddr(true)

	if connected {
		HandshakeNet.connectedToPeer(_conn, addr)
	} else {
		HandshakeNet.disconnectedFromPeer(addr)
	}
}

// unknownPeerHandler notifies of an unknown peer connection attempt
//export unknownPeerHandler
func unknownPeerHandler(_addr *C.netaddr_t, _cert *C.x509_t, _ unsafe.Pointer) {
	addr := salticidae.NetAddrFromC(salticidae.CNetAddr(_addr)).Copy(true)
	ip := toIPDesc(addr)
	HandshakeNet.log.Info("Adding peer %s", ip)

	cert := ids.ShortID{}
	if HandshakeNet.enableStaking {
		cert = getCert(salticidae.X509FromC(salticidae.CX509(_cert)))
	} else {
		cert = toShortID(ip)
	}

	HandshakeNet.reconnectTimeout.Put(cert.LongID(), func() {
		HandshakeNet.net.DelPeer(addr)
	})
	HandshakeNet.net.AddPeer(addr)
}

// ping handles the recept of a ping message
//export ping
func ping(_ *C.struct_msg_t, _conn *C.struct_msgnetwork_conn_t, _ unsafe.Pointer) {
	conn := salticidae.PeerNetworkConnFromC(salticidae.CPeerNetworkConn(_conn))
	addr := conn.GetPeerAddr(false)
	defer addr.Free()
	if addr.IsNull() {
		HandshakeNet.log.Warn("Ping sent from unknown peer")
		return
	}

	build := Builder{}
	pong, err := build.Pong()
	HandshakeNet.log.AssertNoError(err)

	HandshakeNet.send(pong, addr)
}

// pong handles the recept of a pong message
//export pong
func pong(*C.struct_msg_t, *C.struct_msgnetwork_conn_t, unsafe.Pointer) {}

// getVersion handles the recept of a getVersion message
//export getVersion
func getVersion(_msg *C.struct_msg_t, _conn *C.struct_msgnetwork_conn_t, _ unsafe.Pointer) {
	HandshakeNet.numGetVersionReceived.Inc()

	conn := salticidae.PeerNetworkConnFromC(salticidae.CPeerNetworkConn(_conn))
	addr := conn.GetPeerAddr(false)
	defer addr.Free()

	if addr.IsNull() {
		HandshakeNet.log.Warn("GetVersion sent from unknown peer")
		return
	}

	HandshakeNet.SendVersion(addr)
}

// version handles the recept of a version message
//export version
func version(_msg *C.struct_msg_t, _conn *C.struct_msgnetwork_conn_t, _ unsafe.Pointer) {
	HandshakeNet.numVersionReceived.Inc()

	msg := salticidae.MsgFromC(salticidae.CMsg(_msg))
	conn := salticidae.PeerNetworkConnFromC(salticidae.CPeerNetworkConn(_conn))
	addr := conn.GetPeerAddr(true)
	if addr.IsNull() {
		HandshakeNet.log.Warn("Version sent from unknown peer")
		return
	}

	cert := ids.ShortID{}
	if HandshakeNet.enableStaking {
		cert = getMsgCert(_conn)
	} else {
		ip := toIPDesc(addr)
		cert = toShortID(ip)
	}

	defer HandshakeNet.pending.Remove(addr, cert)

	build := Builder{}
	pMsg, err := build.Parse(Version, msg.GetPayloadByMove())
	if err != nil {
		HandshakeNet.log.Warn("Failed to parse Version message")

		HandshakeNet.net.DelPeer(addr)
		return
	}

	if networkID := pMsg.Get(NetworkID).(uint32); networkID != HandshakeNet.networkID {
		HandshakeNet.log.Warn("Peer's network ID doesn't match our networkID: Peer's = %d ; Ours = %d", networkID, HandshakeNet.networkID)

		HandshakeNet.net.DelPeer(addr)
		return
	}

	myTime := float64(HandshakeNet.clock.Unix())
	if peerTime := float64(pMsg.Get(MyTime).(uint64)); math.Abs(peerTime-myTime) > MaxClockDifference.Seconds() {
		HandshakeNet.log.Warn("Peer's clock is too far out of sync with mine. His = %d, Mine = %d (seconds)", uint64(peerTime), uint64(myTime))

		HandshakeNet.net.DelPeer(addr)
		return
	}

	if peerVersion := pMsg.Get(VersionStr).(string); !checkCompatibility(CurrentVersion, peerVersion) {
		HandshakeNet.log.Warn("Bad version")

		HandshakeNet.net.DelPeer(addr)
		return
	}

	HandshakeNet.log.Debug("Finishing handshake with %s", toIPDesc(addr))

	HandshakeNet.SendPeerList(addr)
	HandshakeNet.connections.Add(addr, cert)

	HandshakeNet.versionTimeout.Remove(cert.LongID())

	if !HandshakeNet.enableStaking {
		HandshakeNet.vdrs.Add(validators.NewValidator(cert, 1))
	}

	HandshakeNet.numPeers.Set(float64(HandshakeNet.connections.Len()))

	HandshakeNet.awaitingLock.Lock()
	defer HandshakeNet.awaitingLock.Unlock()

	for i := 0; i < len(HandshakeNet.awaiting); i++ {
		awaiting := HandshakeNet.awaiting[i]
		awaiting.Add(cert)
		if !awaiting.Ready() {
			continue
		}

		newLen := len(HandshakeNet.awaiting) - 1
		HandshakeNet.awaiting[i] = HandshakeNet.awaiting[newLen]
		HandshakeNet.awaiting = HandshakeNet.awaiting[:newLen]

		i--

		go awaiting.Finish()
	}
}

// getPeerList handles the recept of a getPeerList message
//export getPeerList
func getPeerList(_ *C.struct_msg_t, _conn *C.struct_msgnetwork_conn_t, _ unsafe.Pointer) {
	HandshakeNet.numGetPeerlistReceived.Inc()

	conn := salticidae.PeerNetworkConnFromC(salticidae.CPeerNetworkConn(_conn))
	addr := conn.GetPeerAddr(false)
	defer addr.Free()
	if addr.IsNull() {
		HandshakeNet.log.Warn("GetPeerList sent from unknown peer")
		return
	}
	HandshakeNet.SendPeerList(addr)
}

// peerList handles the recept of a peerList message
//export peerList
func peerList(_msg *C.struct_msg_t, _conn *C.struct_msgnetwork_conn_t, _ unsafe.Pointer) {
	HandshakeNet.numPeerlistReceived.Inc()

	msg := salticidae.MsgFromC(salticidae.CMsg(_msg))
	build := Builder{}
	pMsg, err := build.Parse(PeerList, msg.GetPayloadByMove())
	if err != nil {
		HandshakeNet.log.Warn("Failed to parse PeerList message due to %s", err)
		// TODO: What should we do here?
		return
	}

	ips := pMsg.Get(Peers).([]utils.IPDesc)
	cErr := salticidae.NewError()
	for _, ip := range ips {
		HandshakeNet.log.Verbo("Trying to adding peer %s", ip)
		addr := salticidae.NewNetAddrFromIPPortString(ip.String(), true, &cErr)
		if cErr.GetCode() == 0 && !HandshakeNet.myAddr.IsEq(addr) { // Make sure not to connect to myself
			ip := toIPDesc(addr)
			ipCert := toShortID(ip)

			if !HandshakeNet.pending.ContainsIP(addr) && !HandshakeNet.connections.ContainsIP(addr) {
				HandshakeNet.log.Debug("Adding peer %s", ip)

				HandshakeNet.reconnectTimeout.Put(ipCert.LongID(), func() {
					HandshakeNet.net.DelPeer(addr)
				})
				HandshakeNet.net.AddPeer(addr)
			}
		}
	}
}

func getMsgCert(_conn *C.struct_msgnetwork_conn_t) ids.ShortID {
	conn := salticidae.MsgNetworkConnFromC(salticidae.CMsgNetworkConn(_conn))
	return getCert(conn.GetPeerCert())
}

func getPeerCert(_conn *C.struct_peernetwork_conn_t) ids.ShortID {
	conn := salticidae.MsgNetworkConnFromC(salticidae.CMsgNetworkConn(_conn))
	return getCert(conn.GetPeerCert())
}

func getCert(cert salticidae.X509) ids.ShortID {
	der := cert.GetDer(false)
	defer der.Free()

	certDS := salticidae.NewDataStreamMovedFromByteArray(der, false)
	defer certDS.Free()

	certBytes := certDS.GetDataInPlace(certDS.Size()).Get()
	certID, err := ids.ToShortID(hashing.PubkeyBytesToAddress(certBytes))
	HandshakeNet.log.AssertNoError(err)
	return certID
}

// checkCompatibility Check to make sure that the peer and I speak the same language.
func checkCompatibility(myVersion string, peerVersion string) bool {
	// At the moment, we are all compatible.
	return true
}

func toAddr(ip utils.IPDesc, autoFree bool) salticidae.NetAddr {
	err := salticidae.NewError()
	addr := salticidae.NewNetAddrFromIPPortString(ip.String(), autoFree, &err)
	HandshakeNet.log.AssertTrue(err.GetCode() == 0, "IP Failed parsing")
	return addr
}
func toShortID(ip utils.IPDesc) ids.ShortID {
	return ids.NewShortID(hashing.ComputeHash160Array([]byte(ip.String())))
}
