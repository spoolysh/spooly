// Package cloudflare sends mail through Cloudflare Email Service, over its
// SMTP submission: smtp.mx.cloudflare.net on port 465, logged in with an
// API token. The sending domain must be onboarded to Email Sending, which
// needs the Workers Paid plan.
package cloudflare

import "github.com/spoolysh/spooly/internal/delivery/smtp"

// Host and Port are Cloudflare's SMTP submission, TLS from the start.
const (
	Host = "smtp.mx.cloudflare.net"
	Port = 465
)

// New returns a transport logged in with token, a Cloudflare API token with
// the Email Sending: Edit permission. hello is the name to greet with.
func New(token, hello string) *smtp.Transport {
	return smtp.New(smtp.Options{
		Name: "cloudflare", Host: Host, Port: Port, ImplicitTLS: true,
		Username: "api_token", Password: token, Hello: hello,
	})
}
