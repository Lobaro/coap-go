package coap

import (
	"bytes"
	"context"
	"errors"
	"github.com/lobaro/coap-go/coapmsg"
	"sync"
	"time"
)

type Token []byte
type MessageId uint16

type NotifyFunc func(ia *Interaction, currResponse *coapmsg.Message)

// Interaction tracks one interaction between a CoAP client and Server.
// The Interaction is created with a request and ends with a response.
// An interaction is bound to a CoAP Token.
//
// For observe multiple requests (register, deregister)
// and responses (Notifies) might belong to a single interaction
type Interaction struct {
	req              coapmsg.Message // initial request message
	lastMessageId    MessageId       // Last message Id, used to match ACK's
	conn             Connection
	receiveCh        chan *coapmsg.Message
	receiveObserveCh chan *coapmsg.Message

	// isObserve is set to true during a RoundTrip when it was a observe request
	isObserve bool

	// CancelObserve will stop the interaction to listen for Notifications
	StopListenForNotifications context.CancelFunc

	// Channel to hand over raw coap messages from notification updates
	// to the underlying transport where they can be converted into response structs
	NotificationCh chan *coapmsg.Message

	closed      bool
	roundTripMu sync.Mutex
}

type Interactions struct {
	mu           sync.Mutex
	interactions []*Interaction
}

func (ias *Interactions) InteractionCount() int {
	ias.mu.Lock()
	defer ias.mu.Unlock()
	return len(ias.interactions)
}

func (ias *Interactions) RemoveInteraction(interaction *Interaction) {
	ias.mu.Lock()
	defer ias.mu.Unlock()
	for i, ia := range ias.interactions {
		if ia == interaction {
			copy(ias.interactions[i:], ias.interactions[i+1:])
			ias.interactions[len(ias.interactions)-1] = nil // or the zero value of T
			ias.interactions = ias.interactions[:len(ias.interactions)-1]
			return
		}
	}
}

func (ias *Interactions) StartInteraction(conn Connection, reqMsg *coapmsg.Message) *Interaction {
	ias.mu.Lock()
	defer ias.mu.Unlock()
	ia := &Interaction{
		req:              *reqMsg,
		conn:             conn,
		receiveCh:        make(chan *coapmsg.Message, 10),
		receiveObserveCh: make(chan *coapmsg.Message, 10),
	}

	log.WithField("Token", ia.Token()).Debug("Start interaction")

	ias.interactions = append(ias.interactions, ia)

	return ia
}

func (ias *Interactions) FindInteraction(token Token, msgId MessageId) *Interaction {
	ias.mu.Lock()
	defer ias.mu.Unlock()
	for _, ia := range ias.interactions {
		if ia.Token().Equals(token) {
			return ia
		}
		// For empty tokens the message Id must match
		// An ACK/RST is sent by the server as response for a CON but carries no token
		// TODO: Check also message type to only match ACK/RST here?
		if len(token) == 0 && ia.lastMessageId == msgId {
			return ia
		}
	}
	return nil
}

func (ia *Interaction) Token() Token {
	return ia.req.Token
}

func (ia *Interaction) Closed() bool {
	return ia.closed
}

func (ia *Interaction) Close() {
	if ia.closed {
		log.WithField("token", ia.Token()).Warn("Interaction already closed.")
		return
	}
	log.WithField("token", ia.Token()).Debug("Closing interaction.")
	ia.closed = true

	if ia.StopListenForNotifications != nil {
		log.Debug("Stop listening for Notifications.")
		ia.StopListenForNotifications()
	}

	close(ia.receiveCh)
	close(ia.receiveObserveCh)

	ia.conn.RemoveInteraction(ia)
	if ia.conn.InteractionCount() == 0 {
		log.WithField("port", ia.conn.Name()).Debug("No interactions left, closing connection.")
		ia.conn.Close()
	}
}

func isObserveResponse(msg *coapmsg.Message) bool {
	// 3.2.  Notifications
	// Notifications typically have a 2.05 (Content) response code.  They
	// include an Observe Option [...]

	// Non-2.xx responses do not include an Observe Option.
	// TODO: Non-2.xx observe responses are NOT handled correctly at the moment
	return (msg.IsConfirmable() || msg.IsNonConfirmable()) && msg.Options().Get(coapmsg.Observe).IsSet()
}

func (ia *Interaction) HandleMessage(msg *coapmsg.Message) {
	start := time.Now()
	if isObserveResponse(msg) {
		log.WithField("observing", ia.IsObserving()).Debug("Interaction handle observe message...")

		select {
		case ia.receiveObserveCh <- msg:
		default:
			//case <-time.After(1 * time.Second):
			// TODO: We should avoid this. find the reason why it happens and maybe buffer the channel
			log.Error("Interaction did not handled incoming ACK/RST message. Discarding & Close interaction.")
			ia.Close()
		}
	} else {
		log.WithField("observing", ia.IsObserving()).Debug("Interaction handle message...")
		select {
		case ia.receiveCh <- msg:
		default:
			//case <-time.After(1 * time.Second):
			// TODO: We should avoid this. find the reason why it happens and maybe buffer the channel
			log.Error("Interaction did not handled incoming message. Discarding & Close interaction.")
			ia.Close()
		}
	}
	duration := time.Since(start)
	log.WithField("observing", ia.IsObserving()).WithField("duration", duration).Debug("Interaction handle message. DONE.")
}

var READ_MESSAGE_CTX_DONE = errors.New("Read timeout")
var READ_MESSAGE_CHAN_CLOSED = errors.New("Receive channel closed")

func (ia *Interaction) readMessage(ctx context.Context) (*coapmsg.Message, error) {
	select {
	case msg, ok := <-ia.receiveCh:
		if !ok {
			return msg, READ_MESSAGE_CHAN_CLOSED
		}
		return msg, nil
	case <-ctx.Done():
		return nil, READ_MESSAGE_CTX_DONE
	}
}

// readObserveMessage can receive message with the observe option set
func (ia *Interaction) readObserveMessage(ctx context.Context) (*coapmsg.Message, error) {
	select {
	case msg, ok := <-ia.receiveObserveCh:
		if !ok {
			return msg, READ_MESSAGE_CHAN_CLOSED
		}
		return msg, nil
	case <-ctx.Done():
		return nil, READ_MESSAGE_CTX_DONE
	}
}

func (ia *Interaction) IsObserving() bool {
	return ia.isObserve
}

var ERROR_READ_ACK = "Failed to read ACK"

func (ia *Interaction) RoundTrip(ctx context.Context, reqMsg *coapmsg.Message) (resMsg *coapmsg.Message, err error) {
	ia.roundTripMu.Lock()
	defer ia.roundTripMu.Unlock()

	// TODO: The one and only thing we can possibly do while ia.IsObserving() is cancelling the observe
	// This said, all responses MUST be handled by the waitForNotify() method, even an ACK to the cancel request
	// this might make resending the cancelation request a bit more tricky but resending is not implemented yet
	// Any request that does not intent to cancel the observe while ia.IsObserving() should be rejected with an error
	// The intent to cancel the observe must be send to the waitForNotify() method in order to be able to act accordingly

	// This is a cancel observe request.
	if reqMsg.Options().Get(coapmsg.Observe).AsUInt8() > 0 {
		ia.isObserve = false

		// A new round trip on an existing interaction can only work when we are not listening
		// for notifications. Else the notifications eats up all responses from the server.
		// One of the few reason to do this is to cancel an observe anyway
		//
		// We are still able to handle interactions for other tokens in parallel
		//
		// Throws without nil check when requesting unknown resource
		if ia.StopListenForNotifications != nil {
			ia.StopListenForNotifications()
		}
	}

	ia.lastMessageId = MessageId(reqMsg.MessageID)

	// send the request
	err = sendMessage(ia.conn, reqMsg)
	if err != nil {
		return nil, wrapError(err, "Failed to send message")
	}

	if reqMsg.Type == coapmsg.Confirmable {
		// Handle CON request

		// TODO: Implement retries for CON messages until first ACK is received or some timeout
		withAckTimeout, _ := context.WithTimeout(ctx, ackTimeout())
		resMsg, err = ia.readMessage(withAckTimeout)
		if err != nil {
			return resMsg, wrapError(err, ERROR_READ_ACK)
		}
		if err = validateMessageId(reqMsg, resMsg); err != nil {
			return resMsg, wrapError(err, ERROR_READ_ACK)
		}

		// For empty request codes (CoAP ping) we expect a RST
		if reqMsg.Code == coapmsg.Empty && resMsg.Type == coapmsg.Reset {
			return resMsg, nil
		}

		if resMsg.Type != coapmsg.Acknowledgement {
			return resMsg, errors.New("Expected ACK response but got " + resMsg.Type.String())
		}

		// TODO: Handle Types: RST correctly - now we just don't care
		if resMsg.Type == coapmsg.Acknowledgement && resMsg.Code == coapmsg.Empty {
			// Handle postponed (non-piggyback) response

			//  Client              Server
			//    |                  |
			//    |   CON [0x7a10]   |
			//    | GET /temperature |
			//    |   (Token 0x73)   |
			//    +----------------->|
			//    |                  |
			//    |   ACK [0x7a10]   |
			//    |<-----------------+ <- We are here!
			//    |                  |
			//    ... Time Passes  ...
			//    |                  |
			//    |   CON [0x23bb]   |
			//    |   2.05 Content   |
			//    |   (Token 0x73)   |
			//    |     "22.5 C"     |
			//    |<-----------------+
			//    |                  |
			//    |   ACK [0x23bb]   |
			//    +----------------->|
			//    |                  |
			//
			// Figure 5: A GET Request with a Separate Response
			withTimeout, _ := context.WithTimeout(ctx, POSTPONED_RESPONSE_TIMEOUT)
			resMsg, err = ia.readMessage(withTimeout)
			if err != nil {
				return nil, wrapError(err, "Failed to read postponed response")
			}
			// The messageId from resMsg needs to be confirmed
			if resMsg.Type != coapmsg.Confirmable && resMsg.Type != coapmsg.NonConfirmable {
				return nil, errors.New("Expected postponed response [CON or NON] but got " + resMsg.Type.String())
			}
			// TODO: Handle resMsg.Type != coapmsg.Reset - but how? Just okay to return an error?

			if resMsg.Type == coapmsg.Confirmable {
				ack := coapmsg.NewAck(resMsg.MessageID)
				if err := sendMessage(ia.conn, &ack); err != nil {
					return nil, err
				}
			}
		} else if resMsg.Type == coapmsg.Acknowledgement && resMsg.Code != coapmsg.Empty {
			// Handle piggyback response

			// here is no need for
			// separately acknowledging a piggybacked response, as the client will
			// retransmit the request if the Acknowledgement message carrying the
			// piggybacked response is lost.

		} else {
			return nil, errors.New("Received invalid reponse from server")
		}
	} else if reqMsg.Type == coapmsg.NonConfirmable {
		// Handle NON request
		withAckTimeout, _ := context.WithTimeout(ctx, ackTimeout())
		resMsg, err := ia.readMessage(withAckTimeout)
		if err != nil {
			return nil, wrapError(err, "Failed to read NON response")
		}
		if err = validateMessageId(reqMsg, resMsg); err != nil {
			return nil, wrapError(err, "Failed to read NON response")
		}
		if resMsg.Type != coapmsg.NonConfirmable {
			return nil, errors.New("Expected NON response but got " + reqMsg.Type.String())
		}

	} else {
		msgLogEntry(reqMsg).Panic("Invalid request message type from client. Expected CON or NON")
	}

	// Handle observe

	// An observe request must set the observe option to 0
	// the server has to response with the observe option set
	if reqMsg.Options().Get(coapmsg.Observe).IsSet() &&
		reqMsg.Options().Get(coapmsg.Observe).AsUInt8() == 0 &&
		resMsg.Options().Get(coapmsg.Observe).IsSet() {
		ia.isObserve = true
		// Must create chan before returning
		ia.NotificationCh = make(chan *coapmsg.Message, 0)
		go ia.waitForNotify(ctx)
	}

	if err = validateToken(reqMsg, resMsg); err != nil {
		return nil, err
	}
	return resMsg, nil

}

//  Gracefully shut down observe by sending GET with observe=1
// This is the responsibility of the client!
// The interaction will just answer with a NAK to the next notify
/*
func (ia *Interaction) sendCancelObserve() {
	reqMsg := coapmsg.NewMessage()

	reqMsg.Type = coapmsg.NonConfirmable
	reqMsg.Token = ia.Token()
	reqMsg.Code = coapmsg.GET
	reqMsg.Payload = []byte{}
	reqMsg.SetPath(ia.req.Path())
	reqMsg.Options().Set(coapmsg.Observe, 1)
	sendMessage(ia.conn, &reqMsg)
}*/

func (ia *Interaction) handleNotification(resMsg *coapmsg.Message) {
}

// waitForNotify will actively handle notification messages
func (ia *Interaction) waitForNotify(ctx context.Context) {
	defer close(ia.NotificationCh)

	withCancel, cancelCtx := context.WithCancel(ctx)

	logWithToken := log.WithField("token", ia.Token())

	cancelDone := make(chan struct{})
	defer close(cancelDone)
	ia.StopListenForNotifications = func() {
		ia.isObserve = false
		cancelCtx()
		// We must actively wait for the cancel to be done,
		// else readMessage could eat up bytes that it should not
		<-cancelDone
		logWithToken.Info("Stopped to listen for notifications")
	}

	for {
		resMsg, err := ia.readObserveMessage(withCancel)
		if err != nil {
			if err != READ_MESSAGE_CTX_DONE {
				logWithToken.WithError(err).Error("Stopped observer unexpected")
			} else {
				logWithToken.WithError(err).Info("Stopped observer")
			}
			return
		}

		if resMsg.Options().Get(coapmsg.Observe).IsNotSet() {
			log.WithField("msg", resMsg.String()).Error("Got non observe response in observe handler")
		}

		select {
		case ia.NotificationCh <- resMsg:
			// TODO: Should we really only send the ACK when the notification is handled?
			// As it is now, the user might miss a few notifications but can
			// than still attach to the Next channel in the response
			//log.Info("ia.NotificationCh <- resMsg: send ACK")
			if resMsg.Type == coapmsg.Confirmable {
				ack := coapmsg.NewAck(resMsg.MessageID)
				if err := sendMessage(ia.conn, &ack); err != nil {
					logWithToken.WithError(err).Error("Failed to send ACK for notify")
					return
				}
			}
		case <-ctx.Done():
			log.Info("Stopped observer, request context timed out or canceled! Send RST.")
			// Even non-confirmable messages can be answered with a RST
			rst := coapmsg.NewRst(resMsg.MessageID)
			if err := sendMessage(ia.conn, &rst); err != nil {
				logWithToken.WithError(err).Error("Failed to send RST for notify (1)")
				return
			}
			return
		default:
			// Happens when the NotificationCh is closed aka no client is listening
			// This is a bit indirect since the transport has another layer to convert
			// the messages into responses for the client
			logWithToken.Error("No handler for notification messages registered. Send RST.")
			// Even non-confirmable messages can be answered with a RST
			rst := coapmsg.NewRst(resMsg.MessageID)
			if err := sendMessage(ia.conn, &rst); err != nil {
				logWithToken.WithError(err).Error("Failed to send RST for notify (2)")
				return
			}

		}

		// An error response MUST lead to a removal of the observer on server side.
		//
		// [...], in the event that the state of a resource changes in
		// a way that would cause a normal GET request at that time to return a
		// non-2.xx response (for example, when the resource is deleted), the
		// server SHOULD notify the client by sending a notification with an
		// appropriate response code (such as 4.04 Not Found) and subsequently
		// MUST remove the associated entry from the list of observers of the
		// resource.
		if resMsg.Code.IsError() {
			log.WithField("code", resMsg.Code.String()).Info("Stopped observer due to error response from server")
			// No need to send RST anymore but can't harm
			rst := coapmsg.NewRst(resMsg.MessageID)
			if err := sendMessage(ia.conn, &rst); err != nil {
				log.WithError(err).Error("Failed to send RST for notify (3)")
				return
			}
			return
		}
	}

	return
}

func validateMessageId(req, res *coapmsg.Message) error {
	if req.MessageID != res.MessageID {
		// This should never happen
		err := errors.New("coap: MessageId of response does not match")
		log.WithError(err).
			WithField("ReqMessageId", req.MessageID).
			WithField("ResMessageId", res.MessageID).
			WithField("ReqToken", req.Token).
			WithField("ResToken", res.Token).
			Error("An interaction must never be called with the wrong message id")
		return err
	}
	return nil
}

func validateToken(req, res *coapmsg.Message) error {
	if !bytes.Equal(req.Token, res.Token) {
		// This should never happen
		err := errors.New("coap: CRITICAL - Token of response does not match")
		log.WithError(err).
			WithField("ReqMessageId", req.MessageID).
			WithField("ResMessageId", res.MessageID).
			WithField("ReqToken", req.Token).
			WithField("ResToken", res.Token).
			Error("An interaction must never be called with the wrong token")
		return err
	}
	return nil
}

func (t Token) Equals(other Token) bool {
	return bytes.Equal(t, other)
}
