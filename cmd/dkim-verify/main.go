package main

import (
	"log"
	"os"

	"github.com/emersion/go-msgauth/dkim"
)

func main() {
	verifications, err := dkim.Verify(os.Stdin)
	if err != nil {
		log.Fatal(err)
	}

	for _, v := range verifications {
		if v.Err == nil {
			log.Printf("Valid signature for %v", v.Domain)
		} else {
			log.Printf("Invalid signature for %v: %v", v.Domain, v.Err)
		}
		for _, c := range v.HeaderCoverage {
			if c.Present() {
				log.Printf("Signed header field %q (instance at position %d)", c.Key, c.Index)
			} else {
				log.Printf("Oversigned header field %q (no matching instance)", c.Key)
			}
		}
		if v.PolicyErr != nil {
			log.Printf("Header policy not satisfied for %v: %v", v.Domain, v.PolicyErr)
		}
	}
}
