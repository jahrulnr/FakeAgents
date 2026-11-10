package scenario_test

import (
	"math/rand/v2"
	"net/http"
)

func mapHeader(name, value string) http.Header {
	headers := http.Header{}
	headers.Set(name, value)
	return headers
}

func seeded(seed uint64) *rand.Rand {
	return rand.New(rand.NewPCG(seed, seed+1))
}
