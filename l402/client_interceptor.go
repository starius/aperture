package l402

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightninglabs/lndclient"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/zpay32"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const (
	// GRPCErrCode is the error code we receive from a gRPC call if the
	// server expects a payment.
	GRPCErrCode = codes.Internal

	// GRPCErrCodeNew is the new error code we received for a "402 payment
	// required" error with version google.golang.org/grpc v1.41.0. This
	// error should not really be returned by any aperture instance since
	// we never deployed any version with google.golang.org/grpc v1.41.0 in
	// the wild as there were other issues with it as well. So this is just
	// here for completeness and shouldn't be used.
	GRPCErrCodeNew = codes.Unknown

	// GRPCErrMessage is the error message we receive from a gRPC call in
	// conjunction with the GRPCErrCode to signal the client that a payment
	// is required to access the service.
	GRPCErrMessage = "payment required"

	// AuthHeader is is the HTTP response header that contains the payment
	// challenge.
	AuthHeader = "WWW-Authenticate"

	// DefaultMaxCostSats is the default maximum amount in satoshis that we
	// are going to pay for an L402 automatically. Does not include routing
	// fees.
	DefaultMaxCostSats = 1000

	// DefaultMaxRoutingFeeSats is the default maximum routing fee in
	// satoshis that we are going to pay to acquire an L402 token.
	DefaultMaxRoutingFeeSats = 10

	// PaymentTimeout is the maximum time we allow a payment to take before
	// we stop waiting for it.
	PaymentTimeout = 60 * time.Second

	// manualRetryHint is the error text we return to tell the user how a
	// token payment can be retried if the payment fails.
	manualRetryHint = "consider removing pending token file if error " +
		"persists. use 'listauth' command to find out token file name"
)

var (
	// authHeaderRegex is the regular expression the payment challenge must
	// match for us to be able to parse the macaroon and invoice.
	authHeaderRegex = regexp.MustCompile(
		"(LSAT|L402) macaroon=\"(.*?)\", invoice=\"(.*?)\"",
	)

	// errPaymentFailedTerminally is signaled by the payment tracking method
	// to indicate a payment failed for good and will never change to a
	// success state.
	errPaymentFailedTerminally = errors.New("payment is in terminal " +
		"failure state")
)

// ClientInterceptor is a gRPC client interceptor that can handle L402
// authentication challenges with embedded payment requests. It uses a
// connection to lnd to automatically pay for an authentication token.
type ClientInterceptor struct {
	lnd           *lndclient.LndServices
	store         Store
	callTimeout   time.Duration
	maxCost       btcutil.Amount
	maxFee        btcutil.Amount
	lock          sync.Mutex
	allowInsecure bool
}

// NewInterceptor creates a new gRPC client interceptor that uses the provided
// lnd connection to automatically acquire and pay for L402 tokens, unless the
// indicated store already contains a usable token.
func NewInterceptor(lnd *lndclient.LndServices, store Store,
	rpcCallTimeout time.Duration, maxCost,
	maxFee btcutil.Amount, allowInsecure bool) *ClientInterceptor {

	return &ClientInterceptor{
		lnd:           lnd,
		store:         store,
		callTimeout:   rpcCallTimeout,
		maxCost:       maxCost,
		maxFee:        maxFee,
		allowInsecure: allowInsecure,
	}
}

// interceptContext is a struct that contains all information about a call that
// is intercepted by the interceptor.
type interceptContext struct {
	mainCtx  context.Context
	opts     []grpc.CallOption
	metadata *metadata.MD
	token    *Token
}

// UnaryInterceptor is an interceptor method that can be used directly by gRPC
// for unary calls. If the store contains a token, it is attached as credentials
// to every call before patching it through. The response error is also
// intercepted for every call. If there is an error returned and it is
// indicating a payment challenge, a token is acquired and paid for
// automatically. The original request is then repeated back to the server, now
// with the new token attached.
func (i *ClientInterceptor) UnaryInterceptor(ctx context.Context, method string,
	req, reply interface{}, cc *grpc.ClientConn, invoker grpc.UnaryInvoker,
	opts ...grpc.CallOption) error {

	// To avoid paying for a token twice if two parallel requests are
	// happening, we require an exclusive lock here.
	i.lock.Lock()
	defer i.lock.Unlock()

	// Create the context that we'll use to initiate the real request. This
	// contains the means to extract response headers and possibly also an
	// auth token, if we already have paid for one.
	iCtx, err := i.newInterceptContext(ctx, opts)
	if err != nil {
		return err
	}

	// Try executing the call now. If anything goes wrong, we only handle
	// the L402 error message that comes in the form of a gRPC status error.
	rpcCtx, cancel := context.WithTimeout(ctx, i.callTimeout)
	defer cancel()
	err = invoker(rpcCtx, method, req, reply, cc, iCtx.opts...)
	if !IsPaymentRequired(err) {
		return err
	}

	// Find out if we need to pay for a new token or perhaps resume
	// a previously aborted payment.
	err = i.handlePayment(iCtx)
	if err != nil {
		return err
	}

	// Execute the same request again, now with the L402
	// token added as an RPC credential.
	rpcCtx2, cancel2 := context.WithTimeout(ctx, i.callTimeout)
	defer cancel2()
	return invoker(rpcCtx2, method, req, reply, cc, iCtx.opts...)
}

// StreamInterceptor is an interceptor method that can be used directly by gRPC
// for streaming calls. If the store contains a token, it is attached as
// credentials to every stream establishment call before patching it through.
// The response error is also intercepted for every initial stream initiation.
// If there is an error returned and it is indicating a payment challenge, a
// token is acquired and paid for automatically. The original request is then
// repeated back to the server, now with the new token attached.
func (i *ClientInterceptor) StreamInterceptor(ctx context.Context,
	desc *grpc.StreamDesc, cc *grpc.ClientConn, method string,
	streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream,
	error) {

	// To avoid paying for a token twice if two parallel requests are
	// happening, we require an exclusive lock here.
	i.lock.Lock()
	defer i.lock.Unlock()

	// Create the context that we'll use to initiate the real request. This
	// contains the means to extract response headers and possibly also an
	// auth token, if we already have paid for one.
	iCtx, err := i.newInterceptContext(ctx, opts)
	if err != nil {
		return nil, err
	}

	// Try establishing the stream now. If anything goes wrong, we only
	// handle the L402 error message that comes in the form of a gRPC status
	// error. The context of a stream will be used for the whole lifetime of
	// it, so we can't really clamp down on the initial call with a timeout.
	stream, err := streamer(ctx, desc, cc, method, iCtx.opts...)
	if !IsPaymentRequired(err) {
		return stream, err
	}

	// Find out if we need to pay for a new token or perhaps resume
	// a previously aborted payment.
	err = i.handlePayment(iCtx)
	if err != nil {
		return nil, err
	}

	// Execute the same request again, now with the L402 token added
	// as an RPC credential.
	return streamer(ctx, desc, cc, method, iCtx.opts...)
}

// newInterceptContext creates the initial intercept context that can capture
// metadata from the server and sends the local token to the server if one
// already exists.
func (i *ClientInterceptor) newInterceptContext(ctx context.Context,
	opts []grpc.CallOption) (*interceptContext, error) {

	iCtx := &interceptContext{
		mainCtx:  ctx,
		opts:     opts,
		metadata: &metadata.MD{},
	}

	// Let's see if the store already contains a token and what state it
	// might be in. If a previous call was aborted, we might have a pending
	// token that needs to be handled separately.
	var err error
	iCtx.token, err = i.store.CurrentToken()
	switch {
	// If there is no token yet, nothing to do at this point.
	case err == ErrNoToken:

	// Some other error happened that we have to surface.
	case err != nil:
		log.Errorf("Failed to get token from store: %v", err)
		return nil, fmt.Errorf("getting token from store failed: %v",
			err)

	// Only if we have a paid token append it. We don't resume a pending
	// payment just yet, since we don't even know if a token is required for
	// this call. We also never send a pending payment to the server since
	// we know it's not valid.
	case !iCtx.token.isPending():
		if err = i.addL402Credentials(iCtx); err != nil {
			log.Errorf("Adding macaroon to request failed: %v", err)
			return nil, fmt.Errorf("adding macaroon failed: %v",
				err)
		}
	}

	// We need a way to extract the response headers sent by the server.
	// This can only be done through the experimental grpc.Trailer call
	// option. We execute the request and inspect the error. If it's the
	// L402 specific payment required error, we might execute the same
	// method again later with the paid L402 token.
	iCtx.opts = append(iCtx.opts, grpc.Trailer(iCtx.metadata))
	return iCtx, nil
}

// handlePayment tries to obtain a valid token by either tracking the payment
// status of a pending token or paying for a new one.
func (i *ClientInterceptor) handlePayment(iCtx *interceptContext) error {
	switch {
	// Resume/track a pending payment if it was interrupted for some reason.
	case iCtx.token != nil && iCtx.token.isPending():
		log.Infof("Payment of L402 token is required, resuming/" +
			"tracking previous payment from pending L402 token")
		err := i.trackPayment(iCtx.mainCtx, iCtx.token)

		// If the payment failed for good, it will never come back to a
		// success state. We need to remove the pending token and try
		// again.
		if err == errPaymentFailedTerminally {
			iCtx.token = nil
			if err := i.store.RemovePendingToken(); err != nil {
				return fmt.Errorf("error removing pending "+
					"token, cannot retry payment: %v", err)
			}

			// Let's try again by paying for the new token.
			log.Infof("Retrying payment of L402 token invoice")
			var err error
			iCtx.token, err = i.payL402Token(
				iCtx.mainCtx, iCtx.metadata,
			)
			if err != nil {
				return err
			}

			break
		}
		if err != nil {
			return err
		}

	// We don't have a token yet, try to get a new one.
	case iCtx.token == nil:
		// We don't have a token yet, get a new one.
		log.Infof("Payment of L402 token is required, paying invoice")
		var err error
		iCtx.token, err = i.payL402Token(iCtx.mainCtx, iCtx.metadata)
		if err != nil {
			return err
		}

	// We have a token and it's valid, nothing more to do here.
	default:
		log.Debugf("Found valid L402 token to add to request")
	}

	if err := i.addL402Credentials(iCtx); err != nil {
		log.Errorf("Adding macaroon to request failed: %v", err)
		return fmt.Errorf("adding macaroon failed: %v", err)
	}
	return nil
}

// addL402Credentials adds an L402 token to the given intercept context.
func (i *ClientInterceptor) addL402Credentials(iCtx *interceptContext) error {
	if iCtx.token == nil {
		return fmt.Errorf("cannot add nil token to context")
	}

	macaroon, err := iCtx.token.PaidMacaroon()
	if err != nil {
		return err
	}
	iCtx.opts = append(iCtx.opts, grpc.PerRPCCredentials(
		NewMacaroonCredential(macaroon, i.allowInsecure),
	))
	return nil
}

// payL402Token reads the payment challenge from the response metadata and tries
// to pay the invoice encoded in them, returning a paid L402 token if
// successful.
func (i *ClientInterceptor) payL402Token(ctx context.Context, md *metadata.MD) (
	*Token, error) {

	// First parse the authentication header that was stored in the
	// metadata.
	authHeaders := md.Get(AuthHeader)
	if len(authHeaders) == 0 {
		return nil, fmt.Errorf("auth header not found in response")
	}
	// Find the first WWW-Authenticate header, matching authHeaderRegex.
	var matches []string
	for _, authHeader := range authHeaders {
		matches = authHeaderRegex.FindStringSubmatch(authHeader)
		if len(matches) == 4 {
			break
		}
	}
	if len(matches) != 4 {
		return nil, fmt.Errorf("invalid auth header "+
			"format: %s", authHeaders[0])
	}

	// Decode the base64 macaroon and the invoice so we can store the
	// information in our store later.
	macBase64, invoiceStr := matches[2], matches[3]
	macBytes, err := base64.StdEncoding.DecodeString(macBase64)
	if err != nil {
		return nil, fmt.Errorf("base64 decode of macaroon failed: "+
			"%v", err)
	}
	invoice, err := zpay32.Decode(invoiceStr, i.lnd.ChainParams)
	if err != nil {
		return nil, fmt.Errorf("unable to decode invoice: %v", err)
	}

	// Check that the charged amount does not exceed our maximum cost.
	maxCostMsat := lnwire.NewMSatFromSatoshis(i.maxCost)
	if invoice.MilliSat != nil && *invoice.MilliSat > maxCostMsat {
		return nil, fmt.Errorf("cannot pay for L402 automatically, "+
			"cost of %d msat exceeds configured max cost of %d "+
			"msat", *invoice.MilliSat, maxCostMsat)
	}

	// Create and store the pending token so we can resume the payment in
	// case the payment is interrupted somehow.
	token, err := tokenFromChallenge(macBytes, invoice.PaymentHash)
	if err != nil {
		return nil, fmt.Errorf("unable to create token: %v", err)
	}
	err = i.store.StoreToken(token)
	if err != nil {
		return nil, fmt.Errorf("unable to store pending token: %v", err)
	}

	// Pay invoice now and wait for the result to arrive or the main context
	// being canceled.
	payCtx, cancel := context.WithTimeout(ctx, PaymentTimeout)
	defer cancel()
	respChan := i.lnd.Client.PayInvoice(
		payCtx, invoiceStr, i.maxFee, nil,
	)
	select {
	case result := <-respChan:
		if result.Err != nil {
			return nil, result.Err
		}
		token.Preimage = result.Preimage
		token.AmountPaid = lnwire.NewMSatFromSatoshis(result.PaidAmt)
		token.RoutingFeePaid = lnwire.NewMSatFromSatoshis(
			result.PaidFee,
		)
		return token, i.store.StoreToken(token)

	case <-payCtx.Done():
		return nil, fmt.Errorf("payment timed out. try again to track "+
			"payment. %s", manualRetryHint)

	case <-ctx.Done():
		return nil, fmt.Errorf("parent context canceled. try again to"+
			"track payment. %s", manualRetryHint)
	}
}

// trackPayment tries to resume a pending payment by tracking its state and
// waiting for a conclusive result.
func (i *ClientInterceptor) trackPayment(ctx context.Context, token *Token) error {
	// Lookup state of the payment.
	paymentStateCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	payStatusChan, payErrChan, err := i.lnd.Router.TrackPayment(
		paymentStateCtx, token.PaymentHash,
	)
	if err != nil {
		log.Errorf("Could not call TrackPayment on lnd: %v", err)
		return fmt.Errorf("track payment call to lnd failed: %v", err)
	}

	// We can't wait forever, so we give the payment tracking the same
	// timeout as the original payment.
	payCtx, cancel := context.WithTimeout(ctx, PaymentTimeout)
	defer cancel()

	// We'll consume status updates until we reach a conclusive state or
	// reach the timeout.
	for {
		select {
		// If we receive a state without an error, the payment has been
		// initiated. Loop until the payment
		case result := <-payStatusChan:
			switch result.State {
			// If the payment was successful, we have all the
			// information we need and we can return the fully paid
			// token.
			case lnrpc.Payment_SUCCEEDED:
				extractPaymentDetails(token, result)
				return i.store.StoreToken(token)

			// The payment is still in transit, we'll give it more
			// time to complete.
			case lnrpc.Payment_IN_FLIGHT:

			// The payment is in a terminal failed state, it will
			// never recover. There is no use keeping the pending
			// token around. So we signal the caller to remove it
			// and try again.
			case lnrpc.Payment_FAILED:
				return errPaymentFailedTerminally

			// Any other state means either error or timeout.
			default:
				return fmt.Errorf("payment tracking failed "+
					"with state %s. %s",
					result.State.String(), manualRetryHint)
			}

		// Abort the payment execution for any error.
		case err := <-payErrChan:
			return fmt.Errorf("payment tracking failed: %v. %s",
				err, manualRetryHint)

		case <-payCtx.Done():
			return fmt.Errorf("payment tracking timed out. %s",
				manualRetryHint)
		}
	}
}

// IsPaymentRequired inspects an error to find out if it's the specific gRPC
// error returned by the server to indicate a payment is required to access the
// service.
func IsPaymentRequired(err error) bool {
	statusErr, ok := status.FromError(err)
	if !ok {
		return false
	}

	errMsg := strings.ToLower(statusErr.Message())
	return strings.Contains(errMsg, GRPCErrMessage) &&
		(statusErr.Code() == GRPCErrCode ||
			statusErr.Code() == GRPCErrCodeNew)
}

// extractPaymentDetails extracts the preimage and amounts paid for a payment
// from the payment status and stores them in the token.
func extractPaymentDetails(token *Token, status lndclient.PaymentStatus) {
	token.Preimage = status.Preimage
	token.AmountPaid = status.Value
	token.RoutingFeePaid = status.Fee
}
