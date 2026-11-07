// Package migrate is the "gateway migrate" command line: it reads another
// gateway's configuration into an exchange file whose ledger classifies
// every setting, and turns an exchange file into a draft policy. It runs
// offline: it reads only the files it is named, creates only new output
// files, and never activates anything. Settings looser than the source are
// left out unless their ledger item is accepted, and the result passes the
// policy decoder and compiler before it is written.
package migrate
