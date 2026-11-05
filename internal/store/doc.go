// Package store is the gateway's one authoritative database: policy versions,
// draft changes and trials, temporary rules, blocked accounts,
// administrators and the durable receipt of every change, in SQLite with
// write-ahead logging.
//
// It is the only code that runs SQL. It holds the database exclusively and
// locks the deployment, so no second process opens it while it is in use;
// every change commits with its transition record in one transaction, and
// state it cannot verify is reported unavailable, never served. It imports
// only policy and fault. The controller and the local operator commands
// call it; nothing on the request path does.
package store
