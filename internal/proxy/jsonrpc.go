package proxy

import (
	"encoding/json"
	"io"
	"log"
	"net/http"

	"untis-proxy/internal/store"
	"untis-proxy/internal/untis"
)

func (p *Proxy) handleJSONRPC(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	school := r.URL.Query().Get("school")
	if school == "" {
		school = p.opts.School
	}

	switch req.Method {
	case "authenticate":
		p.authenticate(w, r, school, req.ID, req.Params)
	case "logout":
		p.logout(w, r, req.ID)
	case "getTimetable":
		p.getTimetable(w, r, school, req.ID, req.Params, body)
	case "getOwnData", "getTimetable2017", "getLatestImportTime":
		p.writeJSONRPCError(w, req.ID, "Method not found", -32601)
	default:
		p.passthrough(w, r, school, body)
	}
}

func (p *Proxy) authenticate(w http.ResponseWriter, r *http.Request, school string, id json.RawMessage, paramsRaw json.RawMessage) {
	var params struct {
		User     string `json:"user"`
		Password string `json:"password"`
		Client   string `json:"client"`
	}
	_ = json.Unmarshal(paramsRaw, &params)
	if params.User == "" || params.Password == "" {
		p.writeJSONRPCError(w, id, "no username specified", -8502)
		return
	}

	cookie, res, err := p.untis.PasswordLogin(school, params.User, params.Password, params.Client)
	if err != nil {
		if ue, ok := err.(*untis.UpstreamError); ok {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(ue.Raw)
			return
		}
		p.writeJSONRPCError(w, id, "bad credentials", -8504)
		return
	}

	info, _ := p.untis.PersonInfo(school, cookie)
	if info.ClassID == 0 {
		if v, ok := res["klasseId"].(float64); ok {
			info.ClassID = int64(v)
		}
	}
	if info.PersonID == 0 {
		if v, ok := res["personId"].(float64); ok {
			info.PersonID = int64(v)
		}
	}
	if info.PersonType == 0 {
		if v, ok := res["personType"].(float64); ok {
			info.PersonType = int64(v)
		}
	}

	donateClassID := info.ClassID
	user := &store.User{
		Username:    params.User,
		Password:    params.Password,
		Method:      "password",
		School:      school,
		PersonID:    info.PersonID,
		PersonType:  info.PersonType,
		ClassID:     donateClassID,
		ClassName:   p.classNameFor(school, cookie, info.ClassID),
		Email:       info.Email,
		DisplayName: info.DisplayName,
	}
	_ = p.store.UpsertUser(user)
	_ = p.store.Touch(params.User)
	p.stateFor(school)
	if p.isNewSchool(school) {
		p.ensureReconScan(school)
	}
	go p.untis.Logout(school, cookie)

	s := p.sessions.New(params.User, info.ClassID)
	p.setSessionCookies(w, s.ID, school)
	p.writeJSON(w, map[string]any{
		"jsonrpc": "2.0",
		"id":      idVal(id),
		"result": map[string]any{
			"sessionId":  s.ID,
			"personType": info.PersonType,
			"personId":   info.PersonID,
			"klasseId":   info.ClassID,
		},
	})
}

func (p *Proxy) logout(w http.ResponseWriter, r *http.Request, id json.RawMessage) {
	if ck, err := r.Cookie("JSESSIONID"); err == nil {
		p.sessions.Delete(ck.Value)
	}
	p.writeJSON(w, map[string]any{"jsonrpc": "2.0", "id": idVal(id), "result": map[string]any{}})
}

// passthrough forwards an authenticated JSON-RPC request to the real server
// using the requesting user's real session.
func (p *Proxy) passthrough(w http.ResponseWriter, r *http.Request, school string, body []byte) {
	user := p.sessionUser(r)
	if user == nil {
		p.writeJSONRPCError(w, idOf(body), "not logged in", -8520)
		return
	}
	method := extractMethod(body)
	if isWriteMethod(method) && !p.isEditor(user.Username) {
		p.writeJSONRPCError(w, idOf(body), "method not allowed", -32601)
		return
	}
	// jsonrpc_intern.do logs whole bodies; here the body stays out of the log
	// (it can carry messages) but the method name is what makes a missing or
	// refused call identifiable, so record just that.
	log.Printf("[public] %s?%s method=%s user=%s", r.URL.Path, r.URL.RawQuery, method, user.Username)
	// Class-scoped editor data exists upstream only for a teacher identity;
	// editors run those requests as the boosted teacher.
	run := user
	if owner := p.classScopedOwner(school, user.Username, method); owner != nil {
		run = owner
	}
	b, status, err := p.escalatedIntern(school, run, "jsonrpc.do", body)
	if err != nil {
		p.writeJSONRPCError(w, idOf(body), "upstream error", -1)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// extractMethod extracts the JSON-RPC method name from the request body.
func extractMethod(body []byte) string {
	var req struct {
		Method string `json:"method"`
	}
	_ = json.Unmarshal(body, &req)
	return req.Method
}

func idOf(body []byte) json.RawMessage {
	var tmp struct {
		ID json.RawMessage `json:"id"`
	}
	_ = json.Unmarshal(body, &tmp)
	return tmp.ID
}
