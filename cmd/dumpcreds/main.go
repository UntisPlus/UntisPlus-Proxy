package main

import (
	"fmt"
	"os"

	"untis-proxy/internal/store"
)

func main() {
	st, err := store.Open(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	defer st.Close()

	users, err := st.ListUsers()
	if err != nil {
		fmt.Fprintln(os.Stderr, "list:", err)
		os.Exit(1)
	}
	for _, u := range users {
		cred := u.Password
		if s, err := st.GetSecret(u.Username); err == nil && s != "" {
			cred = s
		}
		if cred == "" {
			continue
		}
		fmt.Printf("[%s]\npassword = %s\n\n", u.Username, cred)
	}
}
