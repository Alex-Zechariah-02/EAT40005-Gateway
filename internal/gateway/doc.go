// Package gateway is the security policy gateway: its HTTP server and reverse
// proxy, the request pipeline that applies the five security controls (token
// verification, role permits, rate limits, request validation and the
// security event log), the policy compiler and controller with confirmed
// policy activation and temporary overrides, the SQLite store, the protected
// Admin API and the local operator commands. Main runs the gateway program.
package gateway
