// Package audit is the gateway's durable security log: one JSON record per
// request decision, administrator action and log event, appended to rotating
// files and synced to disk.
//
// An admitted request is acknowledged only after its record is on disk; a
// record that cannot be written fails the log and protected admission stops,
// and records refused meanwhile are counted, never dropped silently. Records
// never hold credentials. The writer never deletes a file: when the file
// limit is reached it fails instead. It imports only fault. The request
// pipeline, the controller and the operator commands write to it.
package audit
