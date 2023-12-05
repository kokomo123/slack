// mautrix-slack - A Matrix-Slack puppeting bridge.
// Copyright (C) 2022 Tulir Asokan
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	_ "net/http/pprof"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	log "maunium.net/go/maulogger/v2"

	"maunium.net/go/mautrix/bridge/status"
	"maunium.net/go/mautrix/id"
)

const (
	SecWebSocketProtocol = "com.gitlab.beeper.slack"
)

type ProvisioningAPI struct {
	bridge *SlackBridge
	log    log.Logger
}

func newProvisioningAPI(br *SlackBridge) *ProvisioningAPI {
	p := &ProvisioningAPI{
		bridge: br,
		log:    br.Log.Sub("Provisioning"),
	}

	prefix := br.Config.Bridge.Provisioning.Prefix

	p.log.Debugln("Enabling provisioning API at", prefix)

	r := br.AS.Router.PathPrefix(prefix).Subrouter()

	r.Use(p.authMiddleware)

	r.HandleFunc("/v1/ping", p.ping).Methods(http.MethodGet)
	r.HandleFunc("/v1/login", p.login).Methods(http.MethodPost)
	r.HandleFunc("/v1/logout", p.logout).Methods(http.MethodPost)
	p.bridge.AS.Router.HandleFunc("/_matrix/app/com.beeper.asmux/ping", p.BridgeStatePing).Methods(http.MethodPost)
	p.bridge.AS.Router.HandleFunc("/_matrix/app/com.beeper.bridge_state", p.BridgeStatePing).Methods(http.MethodPost)

	if p.bridge.Config.Bridge.Provisioning.DebugEndpoints {
		p.log.Debugln("Enabling debug API at /debug")
		r := p.bridge.AS.Router.PathPrefix("/debug").Subrouter()
		r.Use(p.authMiddleware)
		r.PathPrefix("/pprof").Handler(http.DefaultServeMux)
	}

	return p
}

func jsonResponse(w http.ResponseWriter, status int, response interface{}) {
	w.Header().Add("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(response)
}

// Response structs
type Response struct {
	Success bool   `json:"success"`
	Status  string `json:"status"`
}

type Error struct {
	Success bool   `json:"success"`
	Error   string `json:"error"`
	ErrCode string `json:"errcode"`
}

// Wrapped http.ResponseWriter to capture the status code
type responseWrap struct {
	http.ResponseWriter
	statusCode int
}

var _ http.Hijacker = (*responseWrap)(nil)

func (rw *responseWrap) WriteHeader(statusCode int) {
	rw.ResponseWriter.WriteHeader(statusCode)
	rw.statusCode = statusCode
}

func (rw *responseWrap) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := rw.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("response does not implement http.Hijacker")
	}
	return hijacker.Hijack()
}

// Middleware
func (p *ProvisioningAPI) authMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")

		// Special case the login endpoint
		auth = strings.TrimPrefix(auth, "Bearer ")

		if auth != p.bridge.Config.Bridge.Provisioning.SharedSecret {
			jsonResponse(w, http.StatusForbidden, map[string]interface{}{
				"error":   "Invalid auth token",
				"errcode": "M_FORBIDDEN",
			})

			return
		}

		userID := r.URL.Query().Get("user_id")
		user := p.bridge.GetUserByMXID(id.UserID(userID))

		start := time.Now()
		wWrap := &responseWrap{w, 200}
		h.ServeHTTP(wWrap, r.WithContext(context.WithValue(r.Context(), "user", user)))
		duration := time.Since(start).Seconds()

		p.log.Infofln("%s %s from %s took %.2f seconds and returned status %d", r.Method, r.URL.Path, user.MXID, duration, wWrap.statusCode)
	})
}

// websocket upgrader
var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
	Subprotocols: []string{SecWebSocketProtocol},
}

// Handlers
func (p *ProvisioningAPI) ping(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value("user").(*User)

	puppets := []interface{}{}
	for _, team := range user.GetLoggedInTeams() {
		puppets = append(puppets, map[string]interface{}{
			"puppetId":   team.Key.String(),
			"puppetMxid": user.MXID,
			"userId":     team.Key.SlackID,
			"data": map[string]interface{}{
				"team": map[string]string{
					"id":   team.Key.TeamID,
					"name": team.TeamName,
				},
				"self": map[string]string{
					"id":   team.Key.String(),
					"name": team.SlackEmail,
				},
			},
		})
	}

	user.Lock()

	resp := map[string]interface{}{
		"puppets":         puppets,
		"management_room": user.ManagementRoom,
		"mxid":            user.MXID,
	}

	user.Unlock()

	jsonResponse(w, http.StatusOK, resp)
}

func (p *ProvisioningAPI) logout(w http.ResponseWriter, r *http.Request) {
	userID := r.URL.Query().Get("user_id")
	user := p.bridge.GetUserByMXID(id.UserID(userID))

	slackTeamID := strings.Split(r.URL.Query().Get("slack_team_id"), "-")[0] // in case some client sends userTeam instead of team ID

	userTeam := user.GetUserTeam(slackTeamID)
	if userTeam == nil || !userTeam.IsLoggedIn() {
		jsonResponse(w, http.StatusNotFound, Error{
			Error:   "Not logged in",
			ErrCode: "Not logged in",
		})

		return
	}

	err := user.LogoutUserTeam(userTeam)

	if err != nil {
		user.log.Warnln("Error while logging out:", err)

		jsonResponse(w, http.StatusInternalServerError, Error{
			Error:   fmt.Sprintf("Unknown error while logging out: %v", err),
			ErrCode: err.Error(),
		})

		return
	}

	jsonResponse(w, http.StatusOK, Response{true, "Logged out successfully."})
}

func (p *ProvisioningAPI) login(w http.ResponseWriter, r *http.Request) {
	userID := r.URL.Query().Get("user_id")
	user := p.bridge.GetUserByMXID(id.UserID(userID))

	var data struct {
		Token       string
		Cookietoken string
	}

	err := json.NewDecoder(r.Body).Decode(&data)
	if err != nil {
		jsonResponse(w, http.StatusBadRequest, Error{
			Error:   "Invalid JSON",
			ErrCode: "Invalid JSON",
		})
		return
	}

	if data.Token == "" {
		jsonResponse(w, http.StatusBadRequest, Error{
			Error:   "Missing field token",
			ErrCode: "Missing field token",
		})
		return
	}

	if data.Cookietoken == "" {
		jsonResponse(w, http.StatusBadRequest, Error{
			Error:   "Missing field cookietoken",
			ErrCode: "Missing field cookietoken",
		})
		return
	}

	cookieToken, _ := url.PathUnescape(data.Cookietoken)
	info, err := user.TokenLogin(data.Token, cookieToken)
	if err != nil {
		jsonResponse(w, http.StatusNotAcceptable, Error{
			Error:   fmt.Sprintf("Slack login error: %s", err),
			ErrCode: err.Error(),
		})

		return
	}

	jsonResponse(w, http.StatusCreated,
		map[string]interface{}{
			"success": true,
			"teamid":  info.TeamID,
			"userid":  info.UserID,
		})
}

func (p *ProvisioningAPI) BridgeStatePing(w http.ResponseWriter, r *http.Request) {
	if !p.bridge.AS.CheckServerToken(w, r) {
		return
	}
	userID := r.URL.Query().Get("user_id")
	user := p.bridge.GetUserByMXID(id.UserID(userID))
	var global status.BridgeState
	global.StateEvent = status.StateRunning
	global = global.Fill(nil)

	resp := status.GlobalBridgeState{
		BridgeState:  global,
		RemoteStates: map[string]status.BridgeState{},
	}

	userTeams := user.GetLoggedInTeams()
	for _, userTeam := range userTeams {
		var remote status.BridgeState
		if userTeam.IsLoggedIn() {
			remote.StateEvent = status.StateConnected
		} else {
			remote.StateEvent = status.StateLoggedOut
		}
		remote = remote.Fill(userTeam)
		resp.RemoteStates[remote.RemoteID] = remote
	}

	user.log.Debugfln("Responding bridge state in bridge status endpoint: %+v", resp)
	jsonResponse(w, http.StatusOK, &resp)
}
