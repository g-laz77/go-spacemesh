package connectionpool

import (
	"github.com/spacemeshos/go-spacemesh/p2p/p2pcrypto"
	"github.com/spacemeshos/go-spacemesh/p2p/net"

	"bytes"
	"errors"
	"gopkg.in/op/go-logging.v1"
	"sync"
)

type dialResult struct {
	conn net.Connection
	err  error
}

type networker interface {
	Dial(address string, remotePublicKey p2pcrypto.PublicKey) (net.Connection, error) // Connect to a remote node. Can send when no error.
	SubscribeOnNewRemoteConnections() chan net.NewConnectionEvent
	NetworkID() int8
	SubscribeClosingConnections() chan net.Connection
	Logger() *logging.Logger
}

// ConnectionPool stores all net.Connections and make them available to all users of net.Connection.
// There are two sources of connections -
// - Local connections that were created by local node (by calling GetConnection)
// - Remote connections that were provided by a networker impl. in a pub-sub manner
type ConnectionPool struct {
	localPub    p2pcrypto.PublicKey
	net         networker
	connections map[string]net.Connection
	connMutex   sync.RWMutex
	pending     map[string][]chan dialResult
	pendMutex   sync.Mutex
	dialWait    sync.WaitGroup
	shutdown    bool

	newRemoteConn chan net.NewConnectionEvent
	outRemoteConn chan net.NewConnectionEvent
	teardown      chan struct{}
}

// NewConnectionPool creates new ConnectionPool
func NewConnectionPool(network networker, lPub p2pcrypto.PublicKey) *ConnectionPool {
	cPool := &ConnectionPool{
		localPub:      lPub,
		net:           network,
		connections:   make(map[string]net.Connection),
		connMutex:     sync.RWMutex{},
		pending:       make(map[string][]chan dialResult),
		pendMutex:     sync.Mutex{},
		dialWait:      sync.WaitGroup{},
		shutdown:      false,
		newRemoteConn: network.SubscribeOnNewRemoteConnections(),
		outRemoteConn: make(chan net.NewConnectionEvent),
		teardown:      make(chan struct{}),
	}
	go cPool.beginEventProcessing()
	return cPool
}

// Shutdown of the ConnectionPool, gracefully.
// - Close all open connections
// - Waits for all Dial routines to complete and unblock any routines waiting for GetConnection
func (cp *ConnectionPool) Shutdown() {
	cp.connMutex.Lock()
	if cp.shutdown {
		cp.connMutex.Unlock()
		cp.net.Logger().Error("shutdown was already called")
		return
	}
	cp.shutdown = true
	cp.connMutex.Unlock()

	cp.dialWait.Wait()
	cp.teardown <- struct{}{}
	// we won't handle the closing connection events for these connections since we exit the loop once the teardown is done
	cp.closeConnections()
}

func (cp *ConnectionPool) closeConnections() {
	cp.connMutex.Lock()
	// there should be no new connections arriving at this point
	for _, c := range cp.connections {
		c.Close()
	}
	cp.connMutex.Unlock()
}

func (cp *ConnectionPool) handleDialResult(rPub p2pcrypto.PublicKey, result dialResult) {
	cp.pendMutex.Lock()
	for _, p := range cp.pending[rPub.String()] {
		p <- result
	}
	delete(cp.pending, rPub.String())
	cp.pendMutex.Unlock()
}

func compareConnections(conn1 net.Connection, conn2 net.Connection) int {
	return bytes.Compare(conn1.Session().ID().Bytes(), conn2.Session().ID().Bytes())
}

func (cp *ConnectionPool) handleNewConnection(rPub p2pcrypto.PublicKey, newConn net.Connection, source net.ConnectionSource) {
	cp.connMutex.Lock()
	var srcPub, dstPub string
	if source == net.Local {
		srcPub = cp.localPub.String()
		dstPub = rPub.String()
	} else {
		srcPub = rPub.String()
		dstPub = cp.localPub.String()
	}
	cp.net.Logger().Info("new connection %s -> %s. id=%s, sessionID=%v", srcPub, dstPub, newConn.ID(), newConn.Session().ID())
	// check if there isn't already same connection (possible if the second connection is a Remote connection)
	curConn, ok := cp.connections[rPub.String()]
	if ok {
		// it is possible to get a new connection with the same peers as another existing connection, in case the two peers tried to connect to each other at the same time.
		// We need both peers to agree on which connection to keep and which one to close otherwise they might end up closing both connections (bug #195)
		res := compareConnections(curConn, newConn)
		var closeConn net.Connection
		if res <= 0 { // newConn >= curConn
			if res == 0 { // newConn == curConn
				// TODO Is it a potential threat (session hijacking)? Should we keep the existing connection?
				cp.net.Logger().Warning("new connection was created with same session ID as an existing connection, keeping the new connection (assuming existing connection is stale). existing session ID=%v, new session ID=%v, remote=%s", curConn.Session().ID(), newConn.Session().ID(), rPub)
			} else {
				cp.net.Logger().Info("connection created while connection already exists between peers, closing existing connection. existing session ID=%v, new session ID=%v, remote=%s", curConn.Session().ID(), newConn.Session().ID(), rPub)
			}
			closeConn = curConn
			cp.connections[rPub.String()] = newConn
		} else { // newConn < curConn
			cp.net.Logger().Info("connection created while connection already exists between peers, closing new connection. existing session ID=%v, new session ID=%v, remote=%s", curConn.Session().ID(), newConn.Session().ID(), rPub)
			closeConn = newConn
		}
		cp.connMutex.Unlock()
		if closeConn != nil {
			closeConn.Close()
		}

		// we don't need to update on the new connection since there were already a connection in the table and there shouldn't be any registered channel waiting for updates
		return
	}
	cp.connections[rPub.String()] = newConn
	cp.connMutex.Unlock()

	// update all registered channels
	res := dialResult{newConn, nil}
	cp.handleDialResult(rPub, res)
}

func (cp *ConnectionPool) handleClosedConnection(conn net.Connection) {
	cp.net.Logger().Debug("connection %v with %v was closed (sessionID: %v)", conn.String(), conn.RemotePublicKey().String(), conn.Session().ID())
	cp.connMutex.Lock()
	rPub := conn.RemotePublicKey().String()
	cur, ok := cp.connections[rPub]
	// only delete if the closed connection is the same as the cached one (it is possible that the closed connection is a duplication and therefore was closed)
	if ok && cur.ID() == conn.ID() {
		delete(cp.connections, rPub)
	}
	cp.connMutex.Unlock()
}

// GetConnection fetchs or creates if don't exist a connection to the address which is associated with the remote public key
func (cp *ConnectionPool) GetConnection(address string, remotePub p2pcrypto.PublicKey) (net.Connection, error) {
	cp.connMutex.RLock()
	if cp.shutdown {
		cp.connMutex.RUnlock()
		return nil, errors.New("ConnectionPool was shut down")
	}
	// look for the connection in the pool
	conn, found := cp.connections[remotePub.String()]
	if found {
		cp.connMutex.RUnlock()
		return conn, nil
	}
	// register for signal when connection is established - must be called under the connMutex otherwise there is a race
	// where it is possible that the connection will be established and all registered channels will be notified before
	// the current registration
	cp.pendMutex.Lock()
	pendChan := make(chan dialResult)
	_, found = cp.pending[remotePub.String()]
	cp.pending[remotePub.String()] = append(cp.pending[remotePub.String()], pendChan)
	if !found {
		// No one is waiting for a connection with the remote peer, need to call Dial
		go func() {
			cp.dialWait.Add(1)
			conn, err := cp.net.Dial(address, remotePub)
			if err != nil {
				cp.handleDialResult(remotePub, dialResult{nil, err})
			} else {
				cp.handleNewConnection(remotePub, conn, net.Local)
			}
			cp.dialWait.Done()
		}()
	}
	cp.pendMutex.Unlock()
	cp.connMutex.RUnlock()
	// wait for the connection to be established, if the channel is closed (in case of dialing error) will return nil
	res := <-pendChan
	return res.conn, res.err
}

// RemoteConnectionsChannel is a channel that we send processed connections on
func (cp *ConnectionPool) RemoteConnectionsChannel() chan net.NewConnectionEvent {
	return cp.outRemoteConn
}

// GetConnectionIfExists checks if the connection is exists or pending
func (cp *ConnectionPool) GetConnectionIfExists(remotePub p2pcrypto.PublicKey) (net.Connection, error) {
	cp.connMutex.RLock()
	if cp.shutdown {
		cp.connMutex.RUnlock()
		return nil, errors.New("ConnectionPool was shut down")
	}
	// look for the connection in the pool
	if conn, found := cp.connections[remotePub.String()]; found {
		cp.connMutex.RUnlock()
		return conn, nil
	}
	// register for signal when connection is established - must be called under the connMutex otherwise there is a race
	// where it is possible that the connection will be established and all registered channels will be notified before
	// the current registration
	cp.pendMutex.Lock()
	if _, found := cp.pending[remotePub.String()]; !found {
		// No one is waiting for a connection with the remote peer
		cp.pendMutex.Unlock()
		return nil, errors.New("no connection in cpool")
	}

	pendChan := make(chan dialResult)
	cp.pending[remotePub.String()] = append(cp.pending[remotePub.String()], pendChan)
	cp.pendMutex.Unlock()
	cp.connMutex.RUnlock()
	// wait for the connection to be established, if the channel is closed (in case of dialing error) will return nil
	res := <-pendChan
	return res.conn, res.err
}

func (cp *ConnectionPool) beginEventProcessing() {
	closing := cp.net.SubscribeClosingConnections()
Loop:
	for {
		select {
		case nce := <-cp.newRemoteConn:
			cp.handleNewConnection(nce.Conn.RemotePublicKey(), nce.Conn, net.Remote)
			go func(nce net.NewConnectionEvent) { cp.outRemoteConn <- nce }(nce)

		case conn := <-closing:
			cp.handleClosedConnection(conn)

		case <-cp.teardown:
			break Loop
		}
	}
}
