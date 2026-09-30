package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"
	"time"

	"untis-proxy/internal/store"
	"untis-proxy/internal/untis"
)

type entry struct {
	Server, School, Username, Password string
}

func readCreds(path string) []entry {
	b, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("read creds: %v", err)
	}
	var entries []entry
	var cur entry
	for _, raw := range strings.Split(string(b), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if m := regexp.MustCompile(`^\[(.*)\]$`).FindStringSubmatch(line); m != nil {
			if cur.Username != "" {
				entries = append(entries, cur)
			}
			cur = entry{}
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		switch parts[0] {
		case "server":
			cur.Server = parts[1]
		case "school":
			cur.School = parts[1]
		case "username":
			cur.Username = parts[1]
		case "password":
			cur.Password = parts[1]
		}
	}
	if cur.Username != "" {
		entries = append(entries, cur)
	}
	return entries
}

func isKey(secret string) bool {
	return len(secret) == 16 && regexp.MustCompile(`^[A-Z2-7]+$`).MatchString(secret)
}

func main() {
	db := flag.String("db", "untis.db", "sqlite database path")
	creds := flag.String("creds", "credentials.txt", "path to credentials.txt")
	flag.Parse()

	st, err := store.Open(*db)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()

	entries := readCreds(*creds)
	fmt.Printf("seeding %d accounts\n", len(entries))

	for _, e := range entries {
		uc := untis.New(untis.Config{Server: e.Server, School: e.School})
		var cookie string
		var method string
		var secret string
		if isKey(e.Password) {
			method = "key"
			secret = e.Password
			cookie, _, err = uc.KeyLogin(e.School, e.Username, untis.TOTP(secret), time.Now().UnixMilli(), nil)
		} else {
			method = "password"
			secret = e.Password
			cookie, _, err = uc.PasswordLogin(e.School, e.Username, e.Password, "untis-seed")
		}
		if err != nil {
			fmt.Printf("  %-10s FAIL: %v\n", e.Username, err)
			continue
		}
		info, err := uc.PersonInfo(e.School, cookie)
		if err != nil {
			fmt.Printf("  %-10s FAIL (personinfo): %v\n", e.Username, err)
			go uc.Logout(e.School, cookie)
			continue
		}
		u := &store.User{
			Username:    e.Username,
			Password:    secret,
			Method:      method,
			PersonID:    info.PersonID,
			PersonType:  info.PersonType,
			ClassID:     info.ClassID,
			ClassName:   "",
			Email:       info.Email,
			DisplayName: info.DisplayName,
		}
		if err := st.UpsertUser(u); err != nil {
			fmt.Printf("  %-10s FAIL (store): %v\n", e.Username, err)
			continue
		}
		if method == "key" {
			_ = st.UpsertSecret(e.Username, secret)
		}
		fmt.Printf("  %-10s OK  class=%d (%s) person=%d\n", e.Username, info.ClassID, info.ClassName, info.PersonID)
		go uc.Logout(e.School, cookie)
	}
}
