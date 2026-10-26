//go:build !faultinject

package fixture

import "net/http"

func dropAfterWrite(http.ResponseWriter) bool { return false }
