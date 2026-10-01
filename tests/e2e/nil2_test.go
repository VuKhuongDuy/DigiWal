package e2e

import "net/http"

func nil2() *http.Client { return &http.Client{Timeout: 30e9} }
