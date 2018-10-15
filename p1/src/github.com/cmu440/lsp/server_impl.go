// Contains the implementation of a LSP server.

package lsp

import (
	"errors"
	"github.com/cmu440/lspnet"
	"strconv"
	"strings"
	"time"
)

type readReturn struct {
	connID  int
	seqNum  int
	payload []byte
	err     error
}
type connectRequest struct {
	message *Message
	addr    *lspnet.UDPAddr
}

type s_client struct { //server side client structure
	addr          *lspnet.UDPAddr
	seqExpected   int //start with one
	connID        int
	writeSeqNum   int // used for writing, start with 1
	messageToPush *readReturn
	//received data messages that is not read yet, no duplicates
	// seq number of messages in pendingMessages >= seqExpected
	//no corrupted messages as well
	pendingMessages []*Message
	messageChan     chan *Message //receive message from readRoutine
	clientCloseChan chan int

	// this is for the rest of partA
	window              []*windowElem
	windowStart         int
	addToWindowChan     chan []byte
	writeBuffer         []*windowElem
	resendSuccessChan   chan int
	connDropChan        chan int //notify clientMain that connection dropped
	gotMessageChan      chan int //notify clientTime that got message from this client
	aboutToClose        bool
	clientTimeCloseChan chan int
}

type writeAckRequest struct {
	ack    *Message
	client *s_client
}

type windowElem struct {
	seqNum  int
	ackChan chan int
	msg     []byte
}

type writeRequest struct {
	connID  int
	payload []byte
}

type server struct {
	// TODO: implement this!
	serverConn       *lspnet.UDPConn
	serverAddr       *lspnet.UDPAddr
	connectedClients []*s_client
	//start at 1, sequence number of data messages sent from server
	curDataSeqNum int
	//start at 1, connID to be assigned when next new connection is made
	curClientConnID int

	newClientChan           chan *s_client
	connectChan             chan *connectRequest // channel to set up new connections
	readReturnChan          chan *readReturn     //channel to send message to Read() back
	params                  *Params
	writeRequestChan        chan *writeRequest
	writeAckChan            chan *writeAckRequest
	writeBackChan           chan error
	searchClientRequestChan chan *lspnet.UDPAddr
	searchClientReturnChan  chan *s_client
	searchClientCloseChan   chan int
	serverFinishCloseChan   chan int

	clientRemoveChan chan int //client  dropped
	mainCloseChan    chan int
	readCloseChan    chan int
	aboutToClose     bool

	// below is for the rest of partA
	//clientWriteErrorChan chan error

}

// NewServer creates, initiates, and returns a new server. This function should
// NOT block. Instead, it should spawn one or more goroutines (to handle things
// like accepting incoming client connections, triggering epoch events at
// fixed intervals, synchronizing events using a for-select loop like you saw in
// project 0, etc.) and immediately return. It should return a non-nil error if
// there was an error resolving or listening on the specified port number.
func NewServer(port int, params *Params) (Server, error) {
	s := server{
		serverConn:              nil,
		serverAddr:              nil,
		connectedClients:        make([]*s_client, 0),
		curDataSeqNum:           1,
		curClientConnID:         1,
		newClientChan:           make(chan *s_client),
		connectChan:             make(chan *connectRequest),
		readReturnChan:          make(chan *readReturn, 500),
		params:                  params,
		writeRequestChan:        make(chan *writeRequest),
		writeAckChan:            make(chan *writeAckRequest),
		writeBackChan:           make(chan error),
		clientRemoveChan:        make(chan int),
		mainCloseChan:           make(chan int),
		readCloseChan:           make(chan int),
		searchClientCloseChan:   make(chan int),
		searchClientRequestChan: make(chan *lspnet.UDPAddr),
		searchClientReturnChan:  make(chan *s_client),
		serverFinishCloseChan:   make(chan int),
		aboutToClose:            false,
	}
	adr, err := lspnet.ResolveUDPAddr("udp", "localhost:"+strconv.Itoa(port))
	if err != nil {
		return nil, err
	}
	s.serverAddr = adr
	conn, err := lspnet.ListenUDP("udp", s.serverAddr)
	if err != nil {
		return nil, err
	}
	s.serverConn = conn
	go s.mainRoutine()
	go s.readRoutine()
	return &s, nil
}

func (s *server) Read() (int, []byte, error) {
	// TODO: remove this line when you are ready to begin implementing this method.
	message := <-s.readReturnChan
	return message.connID, message.payload, message.err

}

func (s *server) Write(connID int, payload []byte) error {
	request := &writeRequest{
		connID:  connID,
		payload: payload,
	}
	s.writeRequestChan <- request
	err := <-s.writeBackChan
	return err
}

func (s *server) CloseConn(connID int) error {
	s.searchClientCloseChan <- connID
	sClient := <-s.searchClientReturnChan
	if sClient != nil {
		sClient.clientCloseChan <- 1
		return nil
	}
	return errors.New("connID doesn't exist")
}

func (s *server) Close() error {
	s.mainCloseChan <- 1
	<-s.serverFinishCloseChan
	return nil
}

func (s *server) mainRoutine() {
	for {
		select {

		case <-s.mainCloseChan: //close
			for i := 0; i < len(s.connectedClients); i++ {
				s.connectedClients[i].clientCloseChan <- 1
			}
			s.aboutToClose = true
			if len(s.connectedClients) == 0 {
				s.serverConn.Close()
				s.readCloseChan <- 1
				s.serverFinishCloseChan <- 1
				return
			}
		case connID := <-s.searchClientCloseChan:
			sClient := s.searchClientToClose(connID)
			s.searchClientReturnChan <- sClient

		case addr := <-s.searchClientRequestChan:
			c := s.searchClient(addr)
			s.searchClientReturnChan <- c

		case connID := <-s.clientRemoveChan: //gets called after sClient has finished sending all pendingMessages
			for i := 0; i < len(s.connectedClients); i++ {
				if s.connectedClients[i].connID == connID {
					s.connectedClients = append(s.connectedClients[:i], s.connectedClients[i+1:]...)
					break
				}
			}
			if s.aboutToClose && len(s.connectedClients) == 0 {
				s.serverConn.Close()
				s.readCloseChan <- 1
				s.serverFinishCloseChan <- 1
				return
			}
		case request := <-s.connectChan: //set up connection
			message := request.message
			if message.Type == MsgConnect { //start a new server side client
				c := &s_client{ //need to adapt to new struct
					addr:                request.addr,
					seqExpected:         1,
					writeSeqNum:         1,
					connID:              s.curClientConnID,
					messageToPush:       nil,
					pendingMessages:     make([]*Message, 0),
					messageChan:         make(chan *Message),
					clientCloseChan:     make(chan int),
					window:              make([]*windowElem, s.params.WindowSize),
					windowStart:         1,
					addToWindowChan:     make(chan []byte),
					connDropChan:        make(chan int), //notify clientMain that connection dropped
					gotMessageChan:      make(chan int),
					writeBuffer:         make([]*windowElem, 0),
					resendSuccessChan:   make(chan int),
					aboutToClose:        false,
					clientTimeCloseChan: make(chan int),
				}
				s.curClientConnID += 1
				s.connectedClients = append(s.connectedClients, c)
				s.newClientChan <- c //let read routine create ack request
				go c.clientMain(s)
				go c.clientTime(s)
			}

		case request := <-s.writeRequestChan: //deal with Write() request, don't actually send to clients
			// write data to client
			connID := request.connID
			payload := request.payload
			var sClient *s_client = nil
			for i := 0; i < len(s.connectedClients); i++ {
				if s.connectedClients[i].connID == connID {
					sClient = s.connectedClients[i]
					break
				}
			}
			if sClient != nil {

				sClient.addToWindowChan <- payload
				s.writeBackChan <- nil
			} else {
				err := errors.New("This client dropped")
				s.writeBackChan <- err
			}

		// write ack to client when getting a data message
		case ackRequest := <-s.writeAckChan:
			ack := ackRequest.ack
			sClient := ackRequest.client

			byteMessage, err := marshal(ack)
			_ = err                //deal with later?
			message1 := &Message{} //store message

			unmarshal(byteMessage, message1) //unMarshall returns *Message
			s.serverConn.WriteToUDP(byteMessage, sClient.addr)
		}
	}
}
func (s *server) searchClientToClose(connID int) *s_client {
	for i := 0; i < len(s.connectedClients); i++ {
		sClient := s.connectedClients[i]
		if sClient.connID == connID {
			return sClient
		}
	}
	return nil
}

func (s *server) searchClient(addr *lspnet.UDPAddr) *s_client {
	for i := 0; i < len(s.connectedClients); i++ {
		sClient := s.connectedClients[i]
		if strings.Compare(sClient.addr.String(), addr.String()) == 0 {
			return sClient
		}
	}
	return nil
}
func (s *server) readRoutine() {
	for {
		select {
		case <-s.readCloseChan:
			return
		default:
			serverConn := s.serverConn
			b := make([]byte, 2000)
			size, addr, err := serverConn.ReadFromUDP(b)
			if err == nil { //deal with error later
				var message Message           //store message
				unmarshal(b[:size], &message) //unMarshall returns *Message
				if integrityCheck(&message) { //check integrity here with checksum and size
					//notify c.clientTime that got some message from this client
					s.searchClientRequestChan <- addr

					sClient := <-s.searchClientReturnChan
					if sClient != nil {
						sClient.gotMessageChan <- 1

					}
					//deal with differenet types of messages
					if message.Type == MsgData {
						if sClient != nil {
							sClient.messageChan <- &message
							//else if seq <seqExpected, then don't worry about returning it to Read()

						}
					} else if message.Type == MsgConnect {
						request := &connectRequest{
							&message,
							addr,
						}
						//check if the client is already connected on the server end
						//newClient := s.searchClient(addr)
						var newClient *s_client = nil
						if sClient == nil { //first connect message
							s.connectChan <- request
							newClient = <-s.newClientChan //wait for new client from main
						} else {
							newClient = sClient
						}
						//make new server side client struct in mainRoutine
						ack := NewAck(newClient.connID, 0)
						ackRequest := &writeAckRequest{
							ack:    ack,
							client: newClient,
						}
						s.writeAckChan <- ackRequest
						//if its ACK, do sth later for epoch
					} else if message.Type == MsgAck {
						//sClient := s.searchClient(addr)

						if sClient != nil && message.SeqNum != 0 { //check if it's not just a reminder message
							sClient.resendSuccessChan <- message.SeqNum
						}
					}

				}
			}
		}
	}
}
func (c *s_client) alreadyReceived(seq int) bool {
	n := len(c.pendingMessages)
	for i := 0; i < n; i++ {
		if c.pendingMessages[i].SeqNum == seq {
			return true
		}
	}
	return false
}

func (sClient *s_client) resendRoutine(elem *windowElem, s *server) {
	//wrtie to client, potentially sending message to server's main routine to handle

	s.serverConn.WriteToUDP(elem.msg, sClient.addr)
	maxBackOff := s.params.MaxBackOffInterval
	curBackOff := 0
	epochPassed := 0
	timer := time.NewTimer(time.Duration(s.params.EpochMillis) * time.Millisecond)

	for {
		select {
		case <-elem.ackChan:
			return
		case <-timer.C: //resend
			if epochPassed >= curBackOff {
				epochPassed = 0
				s.serverConn.WriteToUDP(elem.msg, sClient.addr)
				if curBackOff == 0 {
					curBackOff = min(curBackOff+1, maxBackOff)
				} else {
					curBackOff = min(curBackOff*2, maxBackOff)
				}
			} else {
				epochPassed += 1
			}

			timer = time.NewTimer(time.Duration(s.params.EpochMillis+500) * time.Millisecond)

		}
	}
}
func (sClient *s_client) clientTime(s *server) {
	epoch := s.params.EpochMillis
	epochLimit := s.params.EpochLimit
	reminderTimer := time.NewTimer(time.Duration(epoch) * time.Millisecond)
	connDropTimer := time.NewTimer(time.Duration(epoch*epochLimit) * time.Millisecond)
	ack := NewAck(sClient.connID, 0) //reminder ack
	msg, err := marshal(ack)         //message to be sent to client
	_ = err
	for {
		select {
		case <-sClient.clientTimeCloseChan:
			return
		case <-reminderTimer.C: //haven't received anything from this client for a epoch
			s.serverConn.WriteToUDP(msg, sClient.addr)
			reminderTimer = time.NewTimer(time.Duration(epoch) * time.Millisecond)
		case <-connDropTimer.C: //connection dropped

			sClient.connDropChan <- 1

		case <-sClient.gotMessageChan: //got sth, reset timmer
			reminderTimer = time.NewTimer(time.Duration(epoch) * time.Millisecond)
			connDropTimer = time.NewTimer(time.Duration(epoch*epochLimit) * time.Millisecond)
		}
	}
}
func (sClient *s_client) checkAllSent(s *server) bool {
	ifAllNil := true
	for i := 0; i < s.params.WindowSize; i++ {
		if sClient.window[i] != nil {
			ifAllNil = false
		}
	}
	if ifAllNil {
		return len(sClient.writeBuffer) == 0
	}
	return false
}
func (sClient *s_client) clientTerminateAll(s *server) { //terminate all routine
	sClient.clientTimeCloseChan <- 1
	s.clientRemoveChan <- sClient.connID //remove it self from connectedClient

}

//would block until Read() is called
//mainly deal with out of order messages on each client
//append out of order messages to pendingMessages, try to push the correct
//message to s.readReturnChan when have one
func (sClient *s_client) clientMain(s *server) {
	for {
		var readReturnChan chan *readReturn
		readReturnChan = nil

		if sClient.messageToPush != nil && sClient.messageToPush.seqNum == sClient.seqExpected {
			readReturnChan = s.readReturnChan
		}

		select {
		case <-sClient.clientCloseChan: //CloseConn or Close called
			//set sth to true
			sClient.aboutToClose = true
			if sClient.checkAllSent(s) { //no resend routine around
				sClient.clientTerminateAll(s)
				return
			}
		case message := <-sClient.messageChan:
			if sClient.aboutToClose == false { //ignore incoming data messages from the client if it's closed here
				ack := NewAck(message.ConnID, message.SeqNum)
				ackRequest := &writeAckRequest{
					ack:    ack,
					client: sClient,
				}
				s.writeAckChan <- ackRequest
				if message.SeqNum > sClient.seqExpected {
					if !sClient.alreadyReceived(message.SeqNum) {
						sClient.pendingMessages = append(sClient.pendingMessages, message)
					}
				} else if message.SeqNum == sClient.seqExpected {
					wrapMessage := &readReturn{
						connID:  message.ConnID,
						seqNum:  message.SeqNum,
						payload: message.Payload,
						err:     nil,
					}
					sClient.messageToPush = wrapMessage
				}
			}
		case readReturnChan <- sClient.messageToPush:
			//if entered here, means we just pushed the message with seqNum
			//client.seqExpected to the main readReturnChan, thus need to update
			//and check whether we have pendingMessages that can be
			// fmt.Println("server: has pushed the message")

			sClient.seqExpected += 1
			//go through pending messages and check if already received the next
			//message in order, check againt client.seqExpected
			sClient.messageToPush = nil
			for i := 0; i < len(sClient.pendingMessages); i++ {
				message := sClient.pendingMessages[i]
				if message.SeqNum == sClient.seqExpected {
					//make sure sending messages out in order
					wrapMessage := &readReturn{
						connID:  message.ConnID,
						seqNum:  message.SeqNum,
						payload: message.Payload,
						err:     nil,
					}
					sClient.messageToPush = wrapMessage
					//cut this message off pendingMessages
					sClient.pendingMessages = append(sClient.pendingMessages[:i], sClient.pendingMessages[i+1:]...)

					break //make sure only push one message to the read()
				}
			}
			if sClient.messageToPush == nil && sClient.aboutToClose { //no more message to Push to Read()
				droppedMsg := &readReturn{
					connID:  sClient.connID,
					seqNum:  -1,
					payload: nil,
					err:     errors.New("This client disconnected"),
				}
				s.readReturnChan <- droppedMsg //might block
				return

			}
		// below two cases are for partA
		case payload := <-sClient.addToWindowChan:
			//don't do Write() application call when closeConn is closed
			if sClient.aboutToClose == false {
				seqNum := sClient.writeSeqNum
				sClient.writeSeqNum += 1
				size := len(payload)
				checksum := makeCheckSum(sClient.connID, seqNum, size, payload)
				original := NewData(sClient.connID, seqNum, size, payload, checksum)
				msg, err := marshal(original)
				if err != nil {
					//don't do anything?
					//s.writeBackChan <- err
					continue
				}

				elem := &windowElem{
					seqNum:  seqNum,
					ackChan: make(chan int),
					msg:     msg,
				}
				// the below condition is ** key **
				if seqNum < sClient.windowStart+s.params.WindowSize && sClient.window[seqNum-sClient.windowStart] == nil {
					// can be put into the window
					sClient.window[seqNum-sClient.windowStart] = elem
					go sClient.resendRoutine(elem, s) // NOTE: the first time sending is also done in resendRoutine
				} else {
					sClient.writeBuffer = append(sClient.writeBuffer, elem)

				}
			}

		case seqNum := <-sClient.resendSuccessChan:
			if seqNum < sClient.windowStart { //if sth already passed
				continue
			}
			index := seqNum - sClient.windowStart
			sClient.window[index].ackChan <- 1 //let resendRoutine for this message stop
			sClient.window[index] = nil
			window := sClient.window

			if sClient.aboutToClose && sClient.checkAllSent(s) { //no more pending messages
				sClient.clientTerminateAll(s)
				return
			}
			//if the flag is true, check if window is all nil, len(writeBuffer ) ==0
			//all resendRoutine should be stopped, and stop the timeRoutine for this client
			//and send itself to s.clientRemoveChan

			if index == 0 { //need to update windowStart
				buffer := sClient.writeBuffer
				offset := 0
				for i := 0; i < s.params.WindowSize; i++ {
					if window[i] == nil {
						offset += 1
					} else {
						break
					}
				}
				// for cleaniness and garbage recollection purpose, remake
				// the window every time we slide the window
				offset = min(sClient.writeSeqNum-sClient.windowStart, offset)
				windowSize := s.params.WindowSize
				newWindow := make([]*windowElem, windowSize)
				for i := offset; i < windowSize; i++ {
					newWindow[i-offset] = window[i]
				}

				// add element in the buffer to the window
				emptyStartIndex := windowSize - offset
				bufferToCopy := min(len(sClient.writeBuffer), offset)
				for i := 0; i < bufferToCopy; i++ {
					newWindow[i+emptyStartIndex] = buffer[i]
					go sClient.resendRoutine(buffer[i], s)
				}
				// shrink the buffer
				newBuffer := buffer[bufferToCopy:]
				//change windowStart
				sClient.windowStart += offset
				//update window, buffer
				sClient.window = newWindow
				sClient.writeBuffer = newBuffer
			}
		case <-sClient.connDropChan: //conneciton dropped

			for i := 0; i < s.params.WindowSize; i++ {
				if sClient.window[i] != nil {
					sClient.window[i].ackChan <- 1 //stop the resend routine

				}
			}
			if sClient.aboutToClose { //if closeConn called
				//ignore pendingMessages
				sClient.clientTerminateAll(s) //might block
				//s.readReturnChan <- droppedMsg
				return
			}
			//regular timeout
			sClient.aboutToClose = true
			if sClient.messageToPush == nil { //no more message to Push to Read()
				droppedMsg := &readReturn{
					connID:  sClient.connID,
					seqNum:  -1,
					payload: nil,
					err:     errors.New("This client disconnected"),
				}
				sClient.clientTerminateAll(s) //might block
				s.readReturnChan <- droppedMsg
				return //terminate clientMain since won't be used anymore
			}

		}
	}
}
