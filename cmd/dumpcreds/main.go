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
		secret, password, _ := st.GetCredential(u.Username)
		// A pre-migration row, or one only ever logged in with a live TOTP, has
		// nothing in the secrets table; fall back to the user row's credential.
		if secret == "" && password == "" {
			if u.Password != "" {
				fmt.Printf("[%s]\npassword = %s\n\n", u.Username, u.Password)
			}
			continue
		}
		fmt.Printf("[%s]\n", u.Username)
		if secret != "" {
			fmt.Printf("key      = %s\n", secret)
		}
		if password != "" {
			fmt.Printf("password = %s\n", password)
		}
		fmt.Println()
	}
}
