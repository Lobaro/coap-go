package coap

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"strings"
	"sync"
	"time"

	"github.com/Lobaro/coap-go/coapmsg"
	"github.com/Sirupsen/logrus"
)

type StopBits byte
type Parity byte

const (
	Stop1     StopBits = 1
	Stop1Half StopBits = 15
	Stop2     StopBits = 2
)

const (
	ParityNone  Parity = 'N'
	ParityOdd   Parity = 'O'
	ParityEven  Parity = 'E'
	ParityMark  Parity = 'M' // parity bit is always 1
	ParitySpace Parity = 'S' // parity bit is always 0
)

const UartScheme = "coap+uart"

// Transport uses a Serial port to communicate via UART (e.g. RS232)
// All Serial parameters can be set on the transport
// The host of the request URL specifies the serial connection, e.g. COM3
// The URI scheme must be coap+uart and valid URIs would be
// coap+uart://COM3/sensors/temperature
// coap+uart://ttyS2/sensors/temperature
// Since we can not have a slash (/) in the host name, on linux systems
// the /dev/ part of the device file handle is added implicitly
// https://tools.ietf.org/html/rfc3986#page-21 allows system specific Host lookups
//
// The URI host can be set to "any" to take the first open port found
type TransportUart struct {
	mu        *sync.Mutex
	lastMsgId uint16 // Sequence counter

	TokenGenerator TokenGenerator
	Connecter      SerialConnecter
}

func NewTransportUart() *TransportUart {
	return &TransportUart{
		mu:             &sync.Mutex{},
		TokenGenerator: NewRandomTokenGenerator(),
		Connecter:      NewUartConnecter(),
	}

}

func msgLogEntry(msg *coapmsg.Message) *logrus.Entry {
	bin := msg.MustMarshalBinary()

	options := logrus.Fields{}
	for id, o := range msg.Options() {
		options["Opt:"+id.String()] = o.String()
	}

	return log.WithField("msg", msg.String()).
		WithField("Bin", bin).
		WithField("OptionCount", len(msg.Options()))

	/* Old when there was no msg.String() impl
	return log.WithField("Code", msg.Code.String()).
		WithField("Type", msg.Type.String()).
		WithField("Token", msg.Token).
		WithField("MessageID", msg.MessageID).
		//WithField("Payload", msg.Payload).
		WithField("OptionCount", len(msg.Options())).
		WithFields(options).
		WithField("Bin", bin)*/
}

func logMsg(msg *coapmsg.Message, info string) {
	msgLogEntry(msg).Debug("CoAP message: " + info)
}

// RoundTrip takes care about one Request / Response roundtrip
// 1) Find / Open new Connection
// 2) Find / Create new interaction
// 3) Use interaction to do the actual RoundTip
// 4a) - No Observe -> Release interaction, close Connection if no interactions running
// 4b) - Observe -> Keep interaction until timeout
func (t *TransportUart) RoundTrip(req *Request) (res *Response, err error) {

	if req == nil {
		return nil, errors.New("coap: Got nil request")
	}

	// The client might set a specific token, e.g. to cancel an observe.
	// If there is no token set we create a random token.
	if len(req.Token) == 0 {
		req.Token = t.TokenGenerator.NextToken()
	}

	reqMsg, err := t.buildRequestMessage(req)
	if err != nil {
		return
	}

	//###########################################
	// Open / Reuse the connection
	//###########################################

	if req.URL == nil {
		return nil, errors.New(fmt.Sprint("coap: Missing request URL"))
	}
	if req.URL.Scheme != UartScheme {
		return nil, errors.New(fmt.Sprint("coap: Invalid URL scheme, expected "+UartScheme+" but got: ", req.URL.Scheme))
	}

	conn, err := t.Connecter.Connect(req.URL.Host)
	if err != nil {
		return
	}

	//###########################################
	// Start an interaction and send the request
	//###########################################

	// Debug output for serial connections only
	if serialCon, ok := conn.(*serialConnection); ok {
		tokens := make([]string, 0)
		for _, ia := range serialCon.interactions {
			tokens = append(tokens, fmt.Sprintf("%v", ia.Token()))
		}
		log.WithField("count", len(serialCon.interactions)).
			WithField("tokens", tokens).
			Debug("Interactions")

	}

	// When canceling an observer we must reuse the interaction
	ia := conn.FindInteraction(req.Token, MessageId(0))
	if ia == nil {
		ia = conn.StartInteraction(conn, reqMsg)
	}

	resMsg, err := ia.RoundTrip(req.Context(), reqMsg)

	if err != nil {
		ia.Close()
		return nil, wrapError(err, fmt.Sprint("Failed Interaction Roundtrip with Token ", ia.Token()))
	}

	//###########################################
	// Build and return the response
	//###########################################

	res = buildResponse(req, resMsg)

	// An observe request must set the observe option to 0
	// the server has to response with the observe option set to != 0
	if ia.IsObserving() {
		// Must create chan before returning
		res.next = make(chan *Response, 0)
		go handleInteractionNotifyMessage(ia, req, res)
	} else {
		ia.Close()
	}

	return res, nil
}

// Takes responsibility to close ia
// res.next will be used to send responses to the client
func handleInteractionNotifyMessage(ia *Interaction, initialReq *Request, initialRes *Response) {
	defer close(initialRes.next)

	// When we close the interaction too early,
	// a potential ACK on the cancel observe request can not be received anymore
	defer time.AfterFunc(3*time.Second, func() {
		// We would expect that everything went good and the ia is already closed
		// but if not help a bit
		if !ia.Closed() {
			ia.Close()
		}
	})

	// TODO: There is no timeout for the NotificationCh
	// this puts all responsibility to stop the observe to the client
	// we should consider some big default timeout (e.g. 5 minutes) to close the interaction
	// when nothing is received
	for {
		// Block till receive or chan is closed, panic if chan is nil
		resMsg, ok := <-ia.NotificationCh
		if ok {
			res := buildResponse(initialReq, resMsg)
			select {
			case initialRes.next <- res: // MUST NOT be buffered, else we can't detect a not listening client
			case <-time.After(5 * time.Second): // Give some time for the client to handle res.Next()
				log.WithField("Token", ia.Token()).Warn("No app handler for notification response registered. Stop listen for notifications.")
				return
			}
		} else {
			// Also happens for all non observe requests since ia.NotificationCh will be closed.
			log.Info("Stopped observer, no more notifies expected.")
			return
		}
	}
}

func buildResponse(req *Request, resMsg *coapmsg.Message) *Response {
	return &Response{
		StatusCode: resMsg.Code.Number(),
		Status:     fmt.Sprintf("%d.%02d %s", resMsg.Code.Class(), resMsg.Code.Detail(), resMsg.Code.String()),
		Body:       ioutil.NopCloser(bytes.NewReader(resMsg.Payload)),
		Options:    resMsg.Options(),
		Request:    req,
	}
}

// BuildMessage creates a coap message based on the request
// Takes care of closing the request body
func (t *TransportUart) buildRequestMessage(req *Request) (*coapmsg.Message, error) {
	defer func() {
		_ = req.Body.Close() // Closed already, ignore error
	}()
	if !ValidMethod(req.Method) {
		return nil, errors.New(fmt.Sprint("coap: Invalid method: ", req.Method))
	}

	msgType := coapmsg.NonConfirmable
	if req.Confirmable {
		msgType = coapmsg.Confirmable
	}

	msg := &coapmsg.Message{
		Code:      methodToCode(req.Method),
		Type:      msgType,
		MessageID: t.nextMessageId(),
		Token:     req.Token,
	}
	msg.SetOptions(req.Options)
	msg.SetPathString(req.URL.EscapedPath())

	msg.Options().Del(coapmsg.URIQuery)
	for _, q := range strings.Split(req.URL.RawQuery, "&") {
		if q != "" {
			err := msg.Options().Add(coapmsg.URIQuery, q)
			if err != nil {
				log.
					WithError(err).
					WithField("option", coapmsg.URIQuery).
					WithField("value", q).
					Warn("Failed to add option value to request")
			}
		}
	}

	buf := &bytes.Buffer{}
	n, err := buf.ReadFrom(req.Body)
	if n > 0 && err != nil && err != io.EOF {
		return nil, err
	}
	msg.Payload = buf.Bytes()

	// Gracefully close the body instead of waiting for the defer
	if err := req.Body.Close(); err != nil {
		return nil, err
	}

	return msg, nil
}

func (t *TransportUart) nextMessageId() uint16 {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastMsgId++
	msgId := t.lastMsgId
	return msgId
}

var methodToCodeTable = map[string]coapmsg.COAPCode{
	"GET":    coapmsg.GET,
	"POST":   coapmsg.POST,
	"PUT":    coapmsg.PUT,
	"DELETE": coapmsg.DELETE,
}

// methodToCode returns the code for a given CoAP method.
// Default is GET, use ValidMethod to ensure a valid method.
func methodToCode(method string) coapmsg.COAPCode {
	if code, ok := methodToCodeTable[method]; ok {
		return code
	}
	return coapmsg.GET
}
