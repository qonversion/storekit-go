package storekit

import (
	"bytes"
	"context"
	"encoding/json"
	"io/ioutil"
	"net/http"
	"unicode"

	"github.com/pkg/errors"
)

const (
	sandboxReceiptVerificationURL    = "https://sandbox.itunes.apple.com/verifyReceipt"
	productionReceiptVerificationURL = "https://buy.itunes.apple.com/verifyReceipt"
)

type client struct {
	verificationURL    string
	autofixEnvironment bool
}

// NewVerificationClient defaults to production verification URL with auto fix
// enabled.
//
// Auto fix automatically handles the incompatible receipt environment error. It
// subsequently gets disabled after the first attempt to avoid unexpected
// looping.
func NewVerificationClient() *client {
	return &client{
		verificationURL:    productionReceiptVerificationURL,
		autofixEnvironment: true,
	}
}

// OnProductionEnv sets the client to use sandbox URL for verification.
func (c *client) OnSandboxEnv() *client {
	c.verificationURL = sandboxReceiptVerificationURL
	return c
}

// OnProductionEnv sets the client to use production URL for verification.
func (c *client) OnProductionEnv() *client {
	c.verificationURL = productionReceiptVerificationURL
	return c
}

// WithoutEnvAutoFix disables automatic handling of incompatible receipt
// environment error.
func (c *client) WithoutEnvAutoFix() *client {
	c.autofixEnvironment = false
	return c
}

func (c *client) isSandbox() bool {
	return c.verificationURL == sandboxReceiptVerificationURL
}

func (c *client) isProduction() bool {
	return c.verificationURL == productionReceiptVerificationURL
}

func (c *client) Verify(ctx context.Context, receiptRequest *ReceiptRequest) (body []byte, resp *ReceiptResponse, err error) {
	// Prepare request:
	reqJSON, err := json.Marshal(receiptRequest)
	if err != nil {
		return nil, nil, errors.Wrap(err, "could not marshal receipt request")
	}
	buf := bytes.NewReader(reqJSON)

	// Dial the App Store server:
	body, resp, err = c.queryStore(ctx, buf, c.verificationURL)
	if err != nil {
		return
	}

	// Resend to the secondary url if the primary one is wrong:
	if c.autofixEnvironment {
		resendNeeded, newUrl := c.checkResendNeeded(resp)

		if resendNeeded {
			buf = bytes.NewReader(reqJSON)
			body, resp, err = c.queryStore(ctx, buf, newUrl)
		}
	}

	return
}

// Send prepared request to Appstore and parse the response:
func (c *client) queryStore(ctx context.Context, requestBuf *bytes.Reader, url string) (body []byte, resp *ReceiptResponse, err error) {
	body, err = c.post(ctx, requestBuf, url)
	if err != nil {
		return
	}

	resp, err = parseResponse(body)
	if err != nil {
		return
	}

	return
}

func parseResponse(body []byte) (*ReceiptResponse, error) {
	resp := &ReceiptResponse{}
	err := json.Unmarshal(
		bytes.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return -1
			}
			return r
		}, body),
		resp,
	)
	if err != nil {
		return nil, errors.Wrap(err, "could not unmarshal app store response")
	}

	return resp, nil
}

func (c *client) post(ctx context.Context, requestBuf *bytes.Reader, url string) ([]byte, error) {
	req, err := http.NewRequest("POST", url, requestBuf)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(ctx)
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		// TODO: Handle this error (and probably retry at least once):
		//       Post https://sandbox.itunes.apple.com/verifyReceipt: read tcp 10.1.11.101:36372->17.154.66.159:443: read: connection reset by peer
		return nil, errors.Wrap(err, "could not connect to app store server")
	}
	if r.StatusCode != http.StatusOK {
		return nil, errors.New("app store http error (" + r.Status + ")")
	}

	// Parse response:
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return nil, errors.Wrap(err, "could not read app store response")
	}

	return body, nil
}

func (c *client) checkResendNeeded(resp *ReceiptResponse) (resendNeeded bool, newUrl string) {
	resendNeeded = false

	switch resp.Status {
	case ReceiptResponseStatusSandboxReceiptSentToProduction:
		// On a 21007 status, retry the request in the sandbox environment:
		if c.isProduction() {
			resendNeeded = true
			newUrl = sandboxReceiptVerificationURL
		}
	case ReceiptResponseStatusProductionReceiptSentToSandbox:
		// On a 21008 status, retry the request in the production environment:
		if c.isSandbox() {
			resendNeeded = true
			newUrl = productionReceiptVerificationURL
		}
	default:
		// TODO: Retry at least once when an App Store internal error occurs here:
		// 	if resp.Status >= 21100 && resp.Status <= 21199 {
		// 		if resp.IsRetryable {
		// 			goto post
		// 		}
		// 	}
		break
	}

	return
}
