// Package mqtt implements MQTT clients and servers
package mqtt

import (
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net"
	"strings"
	"sync"

	proto "github.com/huin/mqtt"
)

type subscriptions struct {
	workers int
	posts   chan (post)

	mu        sync.Mutex // guards access to fields below
	subs      map[string][]*IncomingConn
	wildcards []wild
	// This map needs to hold copies of the proto.Publish, not pointers to
	// it, or else we can send out one with the wrong retain flag.
	retain map[string]proto.Publish
}

// The length of the queue that subscription processing
// workers are taking from.
const postQueue = 100

func newSubscriptions(workers int) *subscriptions {
	s := &subscriptions{
		subs:    make(map[string][]*IncomingConn),
		retain:  make(map[string]proto.Publish),
		posts:   make(chan post, postQueue),
		workers: workers,
	}
	for i := 0; i < s.workers; i++ {
		go s.run(i)
	}
	return s
}

func (s *subscriptions) sendRetain(topic string, c *IncomingConn) {
	s.mu.Lock()
	var tlist []string
	if isWildcard(topic) {
		// TODO: select matching topics from the retain map
	} else {
		tlist = []string{topic}
	}
	for _, t := range tlist {
		if message, ok := s.retain[t]; ok {
			c.submit(&message)
		}
	}
	s.mu.Unlock()
}

func (s *subscriptions) add(topic string, c *IncomingConn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if isWildcard(topic) {
		w := newWild(topic, c)
		if w.valid() {
			s.wildcards = append(s.wildcards, w)
		}
	} else {
		s.subs[topic] = append(s.subs[topic], c)
	}
}

type wild struct {
	wild []string
	c    *IncomingConn
}

func newWild(topic string, c *IncomingConn) wild {
	return wild{wild: strings.Split(topic, "/"), c: c}
}

func (w wild) matches(parts []string) bool {
	i := 0
	for i < len(parts) {
		// topic is longer, no match
		if i >= len(w.wild) {
			return false
		}
		// matched up to here, and now the wildcard says "all others will match"
		if w.wild[i] == "#" {
			return true
		}
		// text does not match, and there wasn't a + to excuse it
		if parts[i] != w.wild[i] && w.wild[i] != "+" {
			return false
		}
		i++
	}

	// make finance/stock/ibm/# match finance/stock/ibm
	if i == len(w.wild)-1 && w.wild[len(w.wild)-1] == "#" {
		return true
	}

	if i == len(w.wild) {
		return true
	}
	return false
}

// Find all connections that are subscribed to this topic.
func (s *subscriptions) subscribers(topic string) []*IncomingConn {
	s.mu.Lock()
	defer s.mu.Unlock()

	// non-wildcard subscribers
	res := s.subs[topic]

	// process wildcards
	parts := strings.Split(topic, "/")
	for _, w := range s.wildcards {
		if w.matches(parts) {
			res = append(res, w.c)
		}
	}

	return res
}

// Remove all subscriptions that refer to a connection.
func (s *subscriptions) unsubAll(c *IncomingConn) {
	s.mu.Lock()
	for _, v := range s.subs {
		for i := range v {
			if v[i] == c {
				v[i] = nil
			}
		}
	}

	// remove any associated entries in the wildcard list
	var wildNew []wild
	for i := 0; i < len(s.wildcards); i++ {
		if s.wildcards[i].c != c {
			wildNew = append(wildNew, s.wildcards[i])
		}
	}
	s.wildcards = wildNew

	s.mu.Unlock()
}

// Remove the subscription to topic for a given connection.
func (s *subscriptions) unsub(topic string, c *IncomingConn) {
	s.mu.Lock()
	if subs, ok := s.subs[topic]; ok {
		nils := 0

		// Search the list, removing references to our connection.
		// At the same time, count the nils to see if this list is now empty.
		for i := 0; i < len(subs); i++ {
			if subs[i] == c {
				subs[i] = nil
			}
			if subs[i] == nil {
				nils++
			}
		}

		if nils == len(subs) {
			delete(s.subs, topic)
		}
	}
	s.mu.Unlock()
}

// The subscription processing worker.
func (s *subscriptions) run(id int) {
	tag := fmt.Sprintf("worker %d ", id)
	log.Print(tag, "started")
	for post := range s.posts {

		// Remember the original retain setting, but send out immediate
		// copies without retain: "When a server sends a PUBLISH to a client
		// as a result of a subscription that already existed when the
		// original PUBLISH arrived, the Retain flag should not be set,
		// regardless of the Retain flag of the original PUBLISH.
		retain := post.m.Header.Retain
		post.m.Header.Retain = false

		// Handle "retain with payload size zero = delete retain".
		// Once the delete is done, return instead of continuing.
		if retain && post.m.Payload.Size() == 0 {
			s.mu.Lock()
			delete(s.retain, post.m.TopicName)
			s.mu.Unlock()
			return
		}

		// Find all the connections that should be notified of this message.
		conns := s.subscribers(post.m.TopicName)
		for _, c := range conns {
			if c != nil {
				c.submit(post.m)
			}
		}

		if retain {
			s.mu.Lock()
			// Save a copy of it, and set that copy's Retain to true, so that
			// when we send it out later we notify new subscribers that this
			// is an old message.
			msg := *post.m
			msg.Header.Retain = true
			s.retain[post.m.TopicName] = msg
			s.mu.Unlock()
		}
	}
}

func (s *subscriptions) submit(c *IncomingConn, m *proto.Publish) {
	s.posts <- post{c: c, m: m}
}

// A post is a unit of work for the subscription processing workers.
type post struct {
	c *IncomingConn
	m *proto.Publish
}

// A Server holds all the state associated with an MQTT server.
type Server struct {
	l    net.Listener
	Done chan struct{}
	subs *subscriptions
}

// NewServer creates a new MQTT server, which accepts connects from
// the given listener. When the server is stopped (for instance by
// another goroutine closing the net.Listener), channel Done will become
// readable.
func NewServer(l net.Listener) *Server {
	return &Server{
		l:    l,
		Done: make(chan struct{}),
		subs: newSubscriptions(2), // 2 workers for now, to see it working in parallel
	}
}

// Start makes the Server start accepting and handling connections.
func (s *Server) Start() {
	go func() {
		for {
			conn, err := s.l.Accept()
			if err != nil {
				log.Print("Accept", err)
				break
			}

			cli := s.NewIncomingConn(conn)
			cli.Start()
		}
		close(s.Done)
	}()
}

// An IncomingConn represents a connection into a Server.
type IncomingConn struct {
	svr      *Server
	conn     net.Conn
	jobs     chan job
	clientid string
	Done     chan struct{}
}

var clients map[string]*IncomingConn = make(map[string]*IncomingConn)
var clientsMu sync.Mutex

const sendingQueueLength = 100

// NewIncomingConn creates a new IncomingConn associated with this
// server. The connection becomes the property of the IncomingConn
// and should not be touched again by the caller until the Done
// channel becomes readable.
func (s *Server) NewIncomingConn(conn net.Conn) *IncomingConn {
	return &IncomingConn{
		svr:  s,
		conn: conn,
		jobs: make(chan job, sendingQueueLength),
		Done: make(chan struct{}),
	}
}

type receipt chan struct{}

// Wait for the receipt to indicate that the job is done.
func (r receipt) wait() {
	// TODO: timeout
	<-r
}

type job struct {
	m proto.Message
	r receipt
}

// Start reading and writing on this connection.
func (c *IncomingConn) Start() {
	go c.reader()
	go c.writer()
}

// Add this	connection to the map, or find out that an existing connection
// already exists for the same client-id.
func (c *IncomingConn) add() *IncomingConn {
	clientsMu.Lock()
	defer clientsMu.Unlock()

	existing, ok := clients[c.clientid]
	if !ok {
		// this client id already exists, return it
		return existing
	}

	clients[c.clientid] = c
	return nil
}

// Delete a connection; the conection must be closed by the caller first.
func (c *IncomingConn) del() {
	clientsMu.Lock()
	defer clientsMu.Unlock()
	delete(clients, c.clientid)
	return
}

// Replace any existing connection with this one. The one to be replaced,
// if any, must be closed first by the caller.
func (c *IncomingConn) replace() {
	clientsMu.Lock()
	defer clientsMu.Unlock()

	// Check that any existing connection is already closed.
	existing, ok := clients[c.clientid]
	if ok {
		die := false
		select {
		case _, ok := <-existing.jobs:
			// what? we are expecting that this channel is closed!
			if ok {
				die = true
			}
		default:
			die = true
		}
		if die {
			panic("attempting to replace a connection that is not closed")
		}

		delete(clients, c.clientid)
	}

	clients[c.clientid] = c
	return
}

// Queue a message; no notification of sending is done.
func (c *IncomingConn) submit(m proto.Message) {
	j := job{m: m}
	c.jobs <- j
	return
}

// Queue a message, returns a channel that will be readable
// when the message is sent.
func (c *IncomingConn) submitSync(m proto.Message) receipt {
	j := job{m: m, r: make(receipt)}
	c.jobs <- j
	return j.r
}

func (c *IncomingConn) reader() {
	// On exit, close the connection and arrange for the writer to exit
	// by closing the output channel.
	defer func() {
		c.conn.Close()
		close(c.jobs)
		log.Print("reader: done")
	}()

	for {
		// TODO: timeout (first message and/or keepalives)
		m, err := proto.DecodeOneMessage(c.conn, nil)
		if err != nil {
			log.Print("reader: ", err)
			return
		}

		// log.Printf("dump: %T", m)

		switch m := m.(type) {
		case *proto.Connect:
			rc := proto.RetCodeAccepted

			if m.ProtocolName != "MQIsdp" ||
				m.ProtocolVersion != 3 {
				log.Print("reader: reject connection from ", m.ProtocolName, " version ", m.ProtocolVersion)
				rc = proto.RetCodeUnacceptableProtocolVersion
			}

			if len(m.ClientId) < 1 || len(m.ClientId) > 23 {
				rc = proto.RetCodeIdentifierRejected
			}
			c.clientid = m.ClientId
			if existing := c.add(); existing != nil {
				disconnect := &proto.Disconnect{}
				r := existing.submitSync(disconnect)
				r.wait()
				existing.del()
			}
			c.add()

			// TODO: Last will

			connack := &proto.ConnAck{
				ReturnCode: rc,
			}
			c.submit(connack)

			// close connection if it was a bad connect
			if rc != proto.RetCodeAccepted {
				return
			}

		case *proto.Publish:
			// TODO: Proper QoS support
			if m.Header.QosLevel != proto.QosAtMostOnce {
				log.Printf("reader: no support for QoS %v yet", m.Header.QosLevel)
				return
			}
			if isWildcard(m.TopicName) {
				log.Print("reader: ignoring PUBLISH with wildcard topic ", m.TopicName)
			} else {
				c.svr.subs.submit(c, m)
			}
			c.submit(&proto.PubAck{MessageId: m.MessageId})

		case *proto.PingReq:
			c.submit(&proto.PingResp{})

		case *proto.Subscribe:
			suback := &proto.SubAck{
				MessageId: m.MessageId,
				TopicsQos: make([]proto.QosLevel, len(m.Topics)),
			}
			for i, tq := range m.Topics {
				// TODO: Handle varying QoS correctly
				c.svr.subs.add(tq.Topic, c)
				suback.TopicsQos[i] = proto.QosAtLeastOnce
			}
			c.submit(suback)

			// Process retained messages.
			for _, tq := range m.Topics {
				c.svr.subs.sendRetain(tq.Topic, c)
			}

		case *proto.Unsubscribe:
			for _, t := range m.Topics {
				c.svr.subs.unsub(t, c)
			}
			ack := &proto.UnsubAck{MessageId: m.MessageId}
			c.submit(ack)

		case *proto.Disconnect:
			return

		default:
			log.Printf("reader: unknown msg type %T", m)
			return
		}
	}
}

func (c *IncomingConn) writer() {

	// Close connection on exit in order to cause reader to exit.
	defer func() {
		c.conn.Close()
		c.del()
		c.svr.subs.unsubAll(c)
		log.Print("writer: done")
	}()

	for job := range c.jobs {
		// TODO: write timeout
		err := job.m.Encode(c.conn)
		if job.r != nil {
			// notifiy the sender that this message is sent
			close(job.r)
		}
		if err != nil {
			// This one is not interesting; it happens when clients
			// disappear before we send their acks.
			if err.Error() == "use of closed network connection" {
				return
			}
			log.Print("writer: ", err)
			return
		}

		if _, ok := job.m.(*proto.Disconnect); ok {
			log.Print("writer: sent disconnect message")
			return
		}
	}
}

// header is used to initialize a proto.Header when the zero value
// is not correct. The zero value of proto.Header is
// the equivalent of header(dupFalse, proto.QosAtMostOnce, retainFalse)
// and is correct for most messages.
func header(d dupFlag, q proto.QosLevel, r retainFlag) proto.Header {
	return proto.Header{
		DupFlag: bool(d), QosLevel: q, Retain: bool(r),
	}
}

type retainFlag bool
type dupFlag bool

const (
	retainFalse retainFlag = false
	retainTrue             = true
	dupFalse    dupFlag    = false
	dupTrue                = true
)

func isWildcard(topic string) bool {
	if strings.Contains(topic, "#") || strings.Contains(topic, "+") {
		return true
	}
	return false
}

func (w wild) valid() bool {
	for i, part := range w.wild {
		// catch things like finance#
		if isWildcard(part) && len(part) != 1 {
			return false
		}
		// # can only occur as the last part
		if part == "#" && i != len(w.wild)-1 {
			return false
		}
	}
	return true
}

const clientQueueLength = 100

type ClientConn struct {
	ClientId string // May be set before the call to Connect
	Incoming chan *proto.Publish
	out      chan job
	conn     net.Conn
	done     chan struct{} // This channel will be readable once a Disconnect has been successfully sent and the connection is closed.
	connack  chan *proto.ConnAck
}

func NewClientConn(c net.Conn) *ClientConn {
	return &ClientConn{
		conn:     c,
		out:      make(chan job, clientQueueLength),
		Incoming: make(chan *proto.Publish, clientQueueLength),
		done:     make(chan struct{}),
		connack:  make(chan *proto.ConnAck),
	}
}

func (c *ClientConn) Start() {
	go c.reader()
	go c.writer()
}

func (c *ClientConn) reader() {
	defer func() {
		c.conn.Close()
		// Cause the writer to exit.
		close(c.out)
		log.Print("cli reader: done")
	}()

	for {
		// TODO: timeout (first message and/or keepalives)
		m, err := proto.DecodeOneMessage(c.conn, nil)
		if err != nil {
			log.Print("cli reader: ", err)
			return
		}

		// log.Printf("dump: %T", m)

		switch m := m.(type) {
		case *proto.Publish:
			c.Incoming <- m
		case *proto.ConnAck:
			c.connack <- m
		case *proto.Disconnect:
			return
		default:
			log.Printf("cli reader: got msg type %T", m)
		}
	}
}

func (c *ClientConn) writer() {
	// Close connection on exit in order to cause reader to exit.
	defer func() {
		c.conn.Close()
		// Signal to Disconnect() that the deed has been done.
		close(c.done)
		log.Print("cli writer: done")
	}()

	for job := range c.out {
		// TODO: write timeout
		err := job.m.Encode(c.conn)
		if job.r != nil {
			close(job.r)
		}

		if err != nil {
			log.Print("cli writer: ", err)
			return
		}

		if _, ok := job.m.(*proto.Disconnect); ok {
			return
		}
	}
}

// Send the CONNECT message to the server. If the ClientId is not already
// set, use a default (a 63-bit decimal random number). The "clean session"
// bit is always set.
func (c *ClientConn) Connect() error {
	// TODO: Keepalive timer
	if c.ClientId == "" {
		c.ClientId = fmt.Sprint(rand.Int63())
	}
	c.sync(&proto.Connect{
		ProtocolName:    "MQIsdp",
		ProtocolVersion: 3,
		ClientId:        c.ClientId,
		CleanSession:    true,
	})
	ack := <-c.connack
	return errs[ack.ReturnCode]
}

var errs = []error{
	nil,
	errors.New("Connection Refused: unacceptable protocol version"),
	errors.New("Connection Refused: identifier rejected"),
	errors.New("Connection Refused: server unavailable"),
	errors.New("Connection Refused: bad user name or password"),
	errors.New("Connection Refused: not authorized"),
}

// Sent a DISCONNECT message to the server. This function blocks until the
// disconnect message is actually sent, and the connection is closed.
func (c *ClientConn) Disconnect() {
	c.sync(&proto.Disconnect{})
	<-c.done
}

func (c *ClientConn) Publish(m *proto.Publish) {
	// TODO: MessageId
	c.out <- job{m: m}
}

// sync sends a message and blocks until it was actually sent.
func (c *ClientConn) sync(m proto.Message) {
	j := job{m: m, r: make(receipt)}
	c.out <- j
	<-j.r
	return
}
