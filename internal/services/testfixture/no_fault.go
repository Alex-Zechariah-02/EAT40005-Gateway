//go:build !faultinject

package testfixture

import "net/http"

func dropAfterWrite(http.ResponseWriter) bool { return false }
