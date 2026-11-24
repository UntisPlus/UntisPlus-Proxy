package store

import (
	"encoding/json"
	"fmt"
)

type LinkedPerson struct {
	ID          int64  `json:"id"`
	Type        int64  `json:"type"`
	DisplayName string `json:"displayName"`
}
type AccountLinks struct {
	ActivePersonID int64          `json:"activePersonId"`
	Persons        []LinkedPerson `json:"persons"`
	Guardian       bool           `json:"guardian"`
}

func (s *Store) SaveAccountLinks(school, username string, activeID int64, payload []byte) error {
	var persons []LinkedPerson
	if err := json.Unmarshal(payload, &persons); err != nil {
		return err
	}
	if len(persons) > 1000 {
		return fmt.Errorf("too many linked persons")
	}
	clean, err := json.Marshal(persons)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO account_person_links(school,username,active_person_id,persons_json) VALUES(?,?,?,?) ON CONFLICT(school,username) DO UPDATE SET active_person_id=excluded.active_person_id,persons_json=excluded.persons_json`, school, norm(username), activeID, string(clean))
	return err
}
func (s *Store) AccountLinks(school, username string) (AccountLinks, error) {
	return (accessReader{s.db}).AccountLinks(school, username)
}

func (r accessReader) AccountLinks(school, username string) (AccountLinks, error) {
	var links AccountLinks
	var raw string
	err := r.QueryRow(`SELECT active_person_id,persons_json FROM account_person_links WHERE school=? AND username=?`, school, norm(username)).Scan(&links.ActivePersonID, &raw)
	if err != nil {
		return links, err
	}
	if err = json.Unmarshal([]byte(raw), &links.Persons); err != nil {
		return AccountLinks{}, err
	}
	// A separate account-level person with linked students is a guardian-style
	// account, not the first child. Never classify it as a teacher source.
	links.Guardian = links.ActivePersonID > 0 && len(links.Persons) > 0
	for _, person := range links.Persons {
		if person.ID == links.ActivePersonID || person.Type != 5 {
			links.Guardian = false
		}
	}
	return links, nil
}
