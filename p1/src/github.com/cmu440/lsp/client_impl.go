// Contains the implementation of a LSP client.

package lsp

import (
	"github.com/cmu440/lspnet"
	//"github.com/cmu440/lspnet" ********* need to use this on autolab
	"encoding/json"
	"fmt"
	"time"
	"errors"
	"strconv"
)


type client struct {
	clientConn  *lspnet.UDPConn
	serverAddr  *lspnet.UDPAddr
	connID      int
	curSeqNum   int
	seqExpected int
	params *Params

	//Read
	messageToPush   *readReturn   //save the one message to return to Read()
	pendingMessages []*Message    //save out of order messages
	messageChan      chan *Message //deal with data messages
	readReturnChan chan *readReturn //channel to send message to Read() back

	//Write

	writeChan         chan []byte // write request sends to this channel
	writeBackChan     chan error  // the chan sent back from main routine
	readChan          chan int    // read request sends to this channel
	payloadChan       chan []byte // where payload is sent from main routine
	writeAckChan      chan int    // ack is going to be sent
	writeConnChan     chan int    // connect is going to be sent
	connIDChan        chan int
	connIDRequestChan chan int // when function connID() calls send data to this channel
	connIDReturnChan  chan int // the function returns value from this channel
	closeChan         chan int
	mainCloseChan     chan int
	readCloseChan     chan int
	timeCloseChan 	  chan int
	// below is for partA
	connDropped bool
	window [] *windowElem // the window that contains all the elements that are trying to resend
    windowStart int
    addToWindowChan chan *windowElem
    resendSuccessChan chan int // index := <- chan, which index from the window start has succeeded
	writeBuffer [] *windowElem

	connDropChan chan int //notify clientMain that connection dropped
	gotMessageChan chan int//notify clientTime that got message from this client
}

// NewClient creates, initiates, and returns a new client. This function
// should return after a connection with the server has been established
// (i.e., the client has received an Ack message from the server in response
// to its connection request), and should return a non-nil error if a
// connection could not be made (i.e., if after K epochs, the client still
// hasn't received an Ack message from the server in response to its K
// connection requests).
//
// hostport is a colon-separated string identifying the server's host address
// and port number (i.e., "localhost:9999").
func NewClient(hostport string, params *Params) (Client, error) {
	serverAddr, err := lspnet.ResolveUDPAddr("udp", hostport)
	// fmt.Println(serverAddr)
	if err != nil {
		return nil, err
	}
	clientConn, err := lspnet.DialUDP("udp", nil, serverAddr)
	if err != nil {
		return nil, err
	}
	// to do: wait to receive ack from server
	c := &client{
		clientConn:        clientConn,
		serverAddr:        serverAddr,
		connID:            -1,
		curSeqNum:         1,
		seqExpected:       1,
		messageToPush:     nil, //save the one message to return to Read()
		messageChan:        make(chan *Message),
		readReturnChan:    make(chan *readReturn), //channel to send message to Read() back
		params: params,

		pendingMessages:   make([]*Message, 0),
		writeChan:         make(chan []byte),
		writeBackChan:     make(chan error),
		readChan:          make(chan int),
		payloadChan:       make(chan []byte),
		writeAckChan:      make(chan int),
		writeConnChan:     make(chan int),
		connIDChan:        make(chan int),
		connIDRequestChan: make(chan int),
		connIDReturnChan:  make(chan int),
		mainCloseChan:     make(chan int),
		readCloseChan:     make(chan int),
		timeCloseChan: 	   make(chan int),
		connDropped: false,
		window: make([] *windowElem, params.WindowSize),// the window that contains all the elements that are trying to resend
    	windowStart: 1,
    	resendSuccessChan: make(chan int),
    	addToWindowChan: make(chan *windowElem),
		connDropChan: make(chan int), //notify clientMain that connection dropped
		gotMessageChan: make(chan int),
		writeBuffer: make([] *windowElem,0),
	}

	go c.mainRoutine()
	go c.readRoutine()
	go c.timeRoutine()
	msg := NewConnect()
	fmt.Println("default windosize is: ", strconv.Itoa(len(c.window)) + "\n")
	//c.writeConnChan <- 1
	byteMsg, err := marshal(msg)
	_ = err
	elem := &windowElem{
			seqNum: 0,
			ackChan: make(chan int),
			msg: byteMsg,
	}
	//assume gonna get ack back
	go c.resendRoutine(elem)//start resend routine for connect
	//insert routine to wait for ack and block later
	// fmt.Println("client: waiting for Ack")
	connID := <-c.connIDChan
	if (connID == 0) {//connection unsuccessful
		//stop read/main routine?
		elem.ackChan <- 1 //stop resending
		//do the same with close read/main routine
		c.Close()
		return nil, errors.New("connection couldn't be made")
	}
	elem.ackChan <- 1 //stop resending
	c.connID = connID

	// fmt.Println("client: newClient() setup done")
	return c, nil
}

func (c *client) ConnID() int {
	c.connIDRequestChan <- 1
	res := <-c.connIDReturnChan
	return res
}

func (c *client) Read() ([]byte, error) {
	message := <-c.readReturnChan
	return message.payload, message.err
}

func (c *client) Write(payload []byte) error {
	// fmt.Println("client: before send write request")
	c.writeChan <- payload
	res := <-c.writeBackChan
	return res
}

func (c *client) Close() error {
	//fmt.Println("Start Close")
	c.mainCloseChan <- 1
	//fmt.Println("Sent close to Main")
	c.readCloseChan <- 1
	//fmt.Println("Sent close to Read")
	c.timeCloseChan <- 1
	//fmt.Println("Sent close to Time")
	return nil
}

// other functions defined below


func marshal(msg *Message) ([]byte, error) {
	res, err := json.Marshal(msg)
	return res, err
}

func unmarshal(data []byte, v *Message) error {
	err := json.Unmarshal(data, v)
	return err
}

func makeCheckSum(connID, seqNum, size int, payload []byte) uint16 {
	connIDSum := Int2Checksum(connID)
	seqNumSum := Int2Checksum(seqNum)
	sizeSum := Int2Checksum(size)

	payloadSum := ByteArray2Checksum(payload)
	// all of these are uint32
	sum := connIDSum + seqNumSum + sizeSum + payloadSum
	for sum > 0xffff {
		carry := sum >> 16
		primary := 0x0000ffff & sum
		sum = carry + primary

	}
	return uint16(sum)
}

func integrityCheck(msg *Message) bool {
	if msg.Type == MsgConnect || msg.Type == MsgAck {
		return true
	}
	actualLen := len(msg.Payload)
	expectedLen := msg.Size
	if actualLen > expectedLen {
		msg.Payload = msg.Payload[:expectedLen]
	}
	actualChecksum := makeCheckSum(msg.ConnID, msg.SeqNum, msg.Size, msg.Payload)
	expectedChecksum := msg.Checksum
	//fmt.Println("")
	return (actualLen >= expectedLen) && (actualChecksum == expectedChecksum)

}
func (c *client) received(seq int) bool {
	n := len(c.pendingMessages)
	for i := 0; i < n; i++ {
		if c.pendingMessages[i].SeqNum == seq {
			return true
		}
	}
	return false
}
func min(x, y int) int {
	if (x < y){
		return x
	}
	return y
	
}
func (c *client) resendRoutine(elem *windowElem) {
	//wrtie to client, potentially sending message to server's main routine to handle
	//fmt.Println("Started resend routine for seqNum: "+strconv.Itoa(elem.seqNum)+" for Client "+strconv.Itoa(c.connID))
	c.clientConn.Write(elem.msg)
	maxBackOff := c.params.MaxBackOffInterval
	curBackOff := 0
	epochPassed := 0
	timer := time.NewTimer(time.Duration(c.params.EpochMillis) * time.Millisecond)

	for {
		select{
		case <- timer.C://resend
			if (epochPassed >= curBackOff){
				epochPassed = 0
				c.clientConn.Write(elem.msg)
				if (curBackOff == 0){ //add one if curBackOff ==1
					curBackOff = min(curBackOff+1,maxBackOff)
				} else { //exponential growth if curBackOff > 0
					curBackOff = min(curBackOff*2,maxBackOff)
				}
			} else{
				epochPassed+=1 //one epoch Passed
			}
			timer = time.NewTimer(time.Duration(c.params.EpochMillis) * time.Millisecond)
		case <- elem.ackChan:
			//fmt.Println("resend Routine ended for message " + strconv.Itoa(elem.seqNum)+"with Client "+strconv.Itoa(c.connID))
			return
		}
	}
}
func (c *client) timeRoutine() {
	epoch := c.params.EpochMillis
	epochLimit := c.params.EpochLimit
	reminderTimer := time.NewTimer(time.Duration(epoch)*time.Millisecond)
	connDropTimer := time.NewTimer(time.Duration(epoch*epochLimit)*time.Millisecond)
	ack := NewAck(c.connID, 0) //reminder ack
	msg, err := marshal(ack) //message to be sent to client
	_ = err
	for {
		select{
		case <- reminderTimer.C://haven't received anything from this client for a epoch 
			c.clientConn.Write(msg)
			reminderTimer = time.NewTimer(time.Duration(epoch)*time.Millisecond)
		case <- connDropTimer.C://connection dropped
			if (c.connID == -1){//still in NewClient() stage waiting for ack
				c.connIDChan <- 0//let NewClient know it failed connecting to server
				return
			}
			c.connDropChan <- 1
			return
		case <- c.gotMessageChan://got sth, reset timmer
			reminderTimer = time.NewTimer(time.Duration(epoch)*time.Millisecond)
			connDropTimer = time.NewTimer(time.Duration(epoch*epochLimit)*time.Millisecond)
		case <- c.timeCloseChan:
			fmt.Println("timeRoutine closed")
			return
		}
	}
}

func (c *client) mainRoutine() {
	for {
		// var readReturnChan chan *readReturn = nil
		// if (c.messageToPush!=nil && c.messageToPush.seqNum==c.seqExpected){
		//     readReturnChan = c.readReturnChan
		// }
		var readReturnChan chan *readReturn
		readReturnChan = nil
		if c.messageToPush != nil && c.messageToPush.seqNum == c.seqExpected {
			readReturnChan = c.readReturnChan
		}
		select {
			case <-c.mainCloseChan:
				for i := 0; i < c.params.WindowSize; i ++ {
						if c.window[i] != nil{
							c.window[i].ackChan <- 1 //stop the resend routine for each message
						} 
					}
				fmt.Println("Main Closed")
				c.clientConn.Close()
				return

			case <- c.connDropChan: //conneciton dropped
				for i := 0; i < c.params.WindowSize; i ++ {
						if c.window[i] != nil{
							c.window[i].ackChan <- 1 //stop the resend routine for each message
						} 
					}
				c.connDropped = true
				droppedMsg := &readReturn{
					connID: c.connID,
					seqNum: 0,
					payload: nil,
					err: errors.New("This client disconnected"),
				}
				c.readReturnChan <- droppedMsg
				

			//write channels called from Write()
			case payload := <- c.writeChan:
				if (c.connDropped) {
					fmt.Println("connection dropped for Client "+strconv.Itoa(c.connID))
					c.writeBackChan <- errors.New("Already disconnected")
					continue
				} else {
					c.writeBackChan <- nil //connection not lost yet
				}
				checksum := makeCheckSum(c.connID, c.curSeqNum, len(payload), payload)
				original := NewData(c.connID, c.curSeqNum, len(payload), payload, checksum)
				msg, err := marshal(original)
				_ = err
				elem := &windowElem{
					seqNum: c.curSeqNum,
					ackChan: make(chan int),
					msg: msg, 
				}
				c.curSeqNum += 1
				//add to window
				seqNum := elem.seqNum
				//fmt.Println("seqNum to write is: " + strconv.Itoa(seqNum) +"\n")
				// the below condition is ** key **

				
				if (seqNum < c.windowStart + c.params.WindowSize && c.window[seqNum - c.windowStart] == nil){
					// can be put into the window
					c.window[seqNum - c.windowStart] = elem
					go c.resendRoutine(elem) // NOTE: the first time sending is also done in resendRoutine
				} else {
					c.writeBuffer = append(c.writeBuffer, elem)
				}

			case seqNum := <- c.resendSuccessChan:
				
				if seqNum < c.windowStart { //got resendSuccess for sth already succeeded
					//could be that alraedy got message so seqNum < windowStart already
					continue
				}
				index := seqNum - c.windowStart
				c.window[index].ackChan <-1 //let resendRoutine for this message stop
				c.window[index] = nil
				window := c.window
				//check if window is all nil and length of writeBuffer is 0, send 1 to timeRoutine  and readRoutine and return
				
				if index == 0{
					offset := 0
					for i := 0; i < c.params.WindowSize; i ++ {
						if window[i] == nil{
							offset += 1
						} else {
							break
						}
					}
					// for cleaniness and garbage recollection purpose, remake 
					// the window every time we slide the window
					offset = min(c.curSeqNum-c.windowStart,offset)
					windowSize := c.params.WindowSize
					newWindow := make([] *windowElem, windowSize)
					for i := offset; i < windowSize; i++{
						newWindow[i - offset] = window[i]
					}
					// add element in the buffer to the window
					emptyStartIndex := windowSize - offset
					//compare number of messages in writeBuffer with offset before copying over
					bufferToCopy := min(len(c.writeBuffer),offset)
					for i := 0; i < bufferToCopy; i ++ {
						newWindow[i + emptyStartIndex] = c.writeBuffer[i]
						go c.resendRoutine(c.writeBuffer[i])
					}
					// shrink the buffer
					newBuffer := c.writeBuffer[bufferToCopy:]
					//change windowStart
					c.windowStart += offset
					//update window, buffer
					c.window = newWindow
					c.writeBuffer = newBuffer
				}

			case seqNum := <-c.writeAckChan:
				ack := NewAck(c.connID, seqNum)
				msg, err := marshal(ack)
				// fmt.Println("client: finished marshal message")
				if err != nil {
					// fmt.Println("client: whoops")
					return
				}
				c.clientConn.Write(msg)
				// fmt.Println("client: ready to send ack")
				


			case <-c.connIDRequestChan:
				c.connIDReturnChan <- c.connID

			//Reading channels, same with server implementation
			case message := <-c.messageChan: // append out of order message
				if message.SeqNum > c.seqExpected {
					if !c.received(message.SeqNum) {
						c.pendingMessages = append(c.pendingMessages, message)
					}
				} else if message.SeqNum == c.seqExpected {
					wrapMessage := &readReturn{
						connID:  message.ConnID,
						seqNum:  message.SeqNum,
						payload: message.Payload,
						err:     nil,
					}
					c.messageToPush = wrapMessage
				}


			case readReturnChan <- c.messageToPush:
				//if entered here, means we just pushed the message with seqNum
				//client.seqExpected to the main readReturnChan, thus need to update
				//and check whether we have pendingMessages that can be
				c.seqExpected += 1
				//go through pending messages and check if already received the next
				//message in order, check againt client.seqExpected
				//fmt.Println("Message pushed: "+ strconv.Itoa(c.seqExpected) +"message is : "+strconv.Itoa(c.messageToPush.seqNum))
				
				c.messageToPush = nil
				for i := 0; i < len(c.pendingMessages); i++ {
					message := c.pendingMessages[i]
					if  message.SeqNum == c.seqExpected {
						//make sure sending messages out in order
						wrapMessage := &readReturn{
							connID:  message.ConnID,
							seqNum:  message.SeqNum,
							payload: message.Payload,
							err:     nil,
						}
						c.messageToPush = wrapMessage
						//cut this message off pendingMessages
						c.pendingMessages = append(c.pendingMessages[:i], c.pendingMessages[i+1:]...)
						break //make sure only push one message to the read()
					}
				}
			
		 
		}
	}
}

func (c *client) readRoutine() {
	for {
		select {
		case <- c.readCloseChan:
			fmt.Println("read Closed")
			return
		default:
			//fmt.Println("Client: started ReadRoutine")
			b := make([]byte, 2000)
			n, err := c.clientConn.Read(b)

			
			//_ = m
			if err == nil { //deal with error later
				var message Message
				// fmt.Println("client: before unmarshal")
				//fmt.Println(b)
				unmarshal(b[:n], &message) //unMarshall returns *Message
				// fmt.Println("client: after unmarshal, integrity checks now")
				actualLen := len(message.Payload)
				expectedLen := message.Size
				//fmt.Println("actualLength is: " + strconv.Itoa(actualLen)+ "expectedLen is : " + strconv.Itoa(expectedLen))
				if actualLen > expectedLen {
					message.Payload = message.Payload[:expectedLen]
				}
				actualChecksum := makeCheckSum(message.ConnID, message.SeqNum, message.Size, message.Payload)
				expectedChecksum := message.Checksum

				if message.Type == MsgConnect || message.Type == MsgAck || ((actualLen >= expectedLen) && (actualChecksum == expectedChecksum)){
					//fmt.Println("message good actualLength is:" + strconv.Itoa(actualLen)+ "expectedLen is :" + strconv.Itoa(expectedLen) + "with Client: " + strconv.Itoa(c.connID))
					//fmt.Println("message has seqNumber: " + strconv.Itoa(message.SeqNum))
					//fmt.Println("\n")
//check integrity here with checksum and size
					//fmt.Println("message Size: " )
					c.gotMessageChan <- 1 //reset timer in timeRoutine, got some message
					if message.Type == MsgData {
						// fmt.Println("client: it's data message!")
						c.messageChan <- &message
						// fmt.Println("client: pushing ack to chan")
						c.writeAckChan <- message.SeqNum //signal to send Ack back
					} else if message.Type == MsgAck {
						// fmt.Println("client: it's ack message!")
						if (message.SeqNum == 0 ){ //ack for connect
							//possible race condition reading c.connID while changing it in newClient()?
							if c.connID== -1 {
								c.connIDChan <- message.ConnID //set up NewClient
							}
						} else {
							//fmt.Println("readRoutine: got ack message with seq:" + strconv.Itoa(message.SeqNum)+"\n")
							//let main routine know that resend was sucessful
							c.resendSuccessChan <- message.SeqNum
						}
					 
					} else {
						fmt.Println("client: this message is wrong type with client" )
					}
					//if its ACK, do sth later for epoch
				} else{

					fmt.Println("client: got corrupted message！with Client: "+ strconv.Itoa(c.connID) +" with Message: " + strconv.Itoa(message.SeqNum))
					//fmt.Println("\n")

				}
			} else if c.connID != -1{ //deal with error when connection set up already
				fmt.Println("client: got error reading!")
				//return //connection lost?
			}
			//fmt.Println("Client: ended ReadRoutine")
		}

	}
}
