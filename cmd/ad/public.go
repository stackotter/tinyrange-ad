package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"time"

	"github.com/gomarkdown/markdown"
	htmlMd "github.com/gomarkdown/markdown/html"
	"github.com/gorilla/websocket"
	"github.com/tinyrange/ad/pkg/htm"
	"github.com/tinyrange/ad/pkg/htm/bootstrap"
	"github.com/tinyrange/ad/pkg/htm/html"
	"github.com/tinyrange/ad/pkg/htm/htmx"
	"github.com/tinyrange/ad/pkg/htm/xtermjs"
	"gopkg.in/yaml.v3"
)

func (game *AttackDefenseGame) isAdmin(r *http.Request) bool {
	return true
}

func (game *AttackDefenseGame) checkForAdmin(w http.ResponseWriter, r *http.Request) bool {
	if !game.isAdmin(r) {
		slog.Warn("attempted to access admin page without permission", "ip", r.RemoteAddr, "path", r.URL.Path)

		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}

	return true
}

func (game *AttackDefenseGame) renderScoreboard() htm.Fragment {
	game.scoreboardMtx.RLock()
	defer game.scoreboardMtx.RUnlock()

	scoreboard := game.OverallState

	if scoreboard == nil {
		return nil
	}

	// Generate table cell contents
	var headerRow htm.Group
	var subheaderRow htm.Group
	headerSpans := []int{}
	headerRow = append(headerRow, html.Text(""))
	subheaderRow = append(subheaderRow,
		html.Text("#"),
		html.Text("Name"),
		html.Text("Points"),
	)
	headerSpans = append(headerSpans, 3)
	for _, service := range game.Config.Vulnbox.PublicServices() {
		headerRow = append(headerRow, html.Textf("%s", service.Name()))
		subheaderRow = append(subheaderRow,
			html.Text("Points"),
			html.Text("Tick"),
			html.Text("Attack"),
			html.Text("Defense"),
			html.Text("Uptime"),
		)
		headerSpans = append(headerSpans, 5)
	}

	var rows []htm.Group
	sortedTeams := slices.Collect(maps.Values(scoreboard.Teams))
	slices.SortFunc(sortedTeams, func(a, b *TeamState) int {
		return a.Position - b.Position
	})
	for _, team := range sortedTeams {
		row := htm.Group{
			html.Textf("%d", team.Position),
			html.Textf("%s", team.Name),
			html.Textf("%.2f", team.Points),
		}

		for _, service := range game.Config.Vulnbox.PublicServices() {
			serviceState := team.Services[service.Id]
			if serviceState == nil {
				row = append(row, html.Text(""), html.Text(""), html.Text(""), html.Text(""), html.Text(""))
			} else {
				row = append(row,
					html.Textf("%.2f", serviceState.Points),
					html.Textf("%.2f", serviceState.TickPoints),
					html.Textf("%.2f", serviceState.AttackPoints),
					html.Textf("%.2f", serviceState.DefensePoints),
					html.Textf("%d%%", int(serviceState.UptimePoints*100)),
				)
			}
		}

		rows = append(rows, row)
	}

	// Render the table
	var headerItems []htm.Fragment
	for i, item := range headerRow {
		headerItems = append(headerItems, htm.NewHtmlFragment("th",
			htm.Attr("colspan", strconv.Itoa(headerSpans[i])),
			htm.Attr("style", "text-align: center"),
			item,
		))
	}

	var colGroups []htm.Fragment
	for i, span := range headerSpans {
		// Add borders between column groups (services)
		style := ""
		if i != len(headerSpans)-1 {
			style = "border-right: 1px solid var(--bs-table-border-color)"
		}

		colGroups = append(colGroups, htm.NewHtmlFragment("colgroup",
			htm.Attr("span", strconv.Itoa(span)),
			htm.Attr("style", style),
		))
	}

	var subheaderItems []htm.Fragment
	for _, item := range subheaderRow {
		subheaderItems = append(subheaderItems, htm.NewHtmlFragment("th", item))
	}

	var rowItems []htm.Fragment
	for _, item := range rows {
		var row htm.Group
		for _, cell := range item {
			row = append(row, htm.NewHtmlFragment("td", cell))
		}
		rowItems = append(rowItems, htm.NewHtmlFragment("tr", row))
	}

	var fragments []htm.Fragment
	fragments = append(fragments,
		htm.Class("table"),
		htm.Class("table-striped"),
	)
	for _, colGroup := range colGroups {
		fragments = append(fragments, colGroup)
	}
	fragments = append(fragments,
		htm.NewHtmlFragment("thead",
			htm.NewHtmlFragment("tr", headerItems...),
			htm.NewHtmlFragment("tr", subheaderItems...),
		),
		htm.NewHtmlFragment("tbody", rowItems...),
	)

	return html.Div(
		htm.Class("table-responsive"),
		htm.NewHtmlFragment("table",
			fragments...,
		),
	)
}

func (game *AttackDefenseGame) renderPage(ctx context.Context, path string) (htm.Fragment, error) {
	page, ok := game.Config.Pages[path]
	if !ok {
		return nil, nil
	}

	pagePath := game.ResolvePath(page.Path)

	pageContent, err := os.ReadFile(pagePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read page: %w", err)
	}

	doc := markdown.Parse(pageContent, nil)

	renderer := htmlMd.NewRenderer(htmlMd.RendererOptions{})

	body := markdown.Render(doc, renderer)

	return htm.UnsafeRawHTML(body), nil
}

var upgrader = websocket.Upgrader{}

func (game *AttackDefenseGame) publicPageError(err error) htm.Fragment {
	return game.publicPageLayout("Error", bootstrap.Alert(bootstrap.AlertColorDanger, htm.Text(err.Error())))
}

func (game *AttackDefenseGame) publicPageLayout(title string, body ...htm.Fragment) htm.Fragment {
	return html.Html(
		htm.Attr("lang", "en"),
		html.Head(
			html.MetaCharset("UTF-8"),
			html.Title(fmt.Sprintf("%s - %s", game.Config.Title, title)),
			html.MetaViewport("width=device-width, initial-scale=1"),
			bootstrap.CSSSrc,
			bootstrap.JavaScriptSrc,
			bootstrap.ColorPickerSrc,
			htmx.JavaScriptSrc,
		),
		html.Body(
			bootstrap.Navbar(
				bootstrap.NavbarBrand("/", html.Text(game.Config.Title)),
				bootstrap.NavbarLink("/scoreboard", html.Text("Scoreboard")),
				bootstrap.NavbarLink("/instances", html.Text("Instances")),
				bootstrap.NavbarLink("/events", html.Text("Events")),
				bootstrap.NavbarLink("/devices", html.Text("Devices")),
				bootstrap.NavbarLink("/config", html.Text("Config")),
			),
			html.Div(bootstrap.Container, htm.Group(body)),
		),
	)
}

func (game *AttackDefenseGame) renderError(w http.ResponseWriter, r *http.Request, err error) {
	if err := htm.Render(r.Context(), w, game.publicPageError(err)); err != nil {
		slog.Error("failed to render page", "err", err)
	}
}

func (game *AttackDefenseGame) startPublicServer() error {
	handler := http.NewServeMux()

	handler.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, err := game.renderPage(r.Context(), "/")
		if err != nil {
			slog.Error("failed to render page", "err", err)
			if err := htm.Render(r.Context(), w, game.publicPageError(err)); err != nil {
				slog.Error("failed to render page", "err", err)
			}
			return
		}

		page := game.publicPageLayout("Home", body)

		if err := htm.Render(r.Context(), w, page); err != nil {
			slog.Error("failed to render page", "err", err)
		}
	})

	// GET /instances lists all running TinyRange instances and provides a button to SSH via WebSSH.
	handler.HandleFunc("GET /instances", func(w http.ResponseWriter, r *http.Request) {
		if !game.checkForAdmin(w, r) {
			return
		}

		instances := game.getInstances()

		var instanceList []htm.Fragment

		for _, instance := range instances {
			instanceList = append(instanceList, html.Div(
				bootstrap.Card(
					bootstrap.CardTitle(instance.Hostname()),
					bootstrap.CardTitle(instance.InstanceAddress().String()),
					bootstrap.LinkButton("/connect/"+instance.Hostname(), bootstrap.ButtonColorPrimary, html.Text("Connect")),
				),
			))
		}

		page := game.publicPageLayout("Instances", instanceList...)

		if err := htm.Render(r.Context(), w, page); err != nil {
			slog.Error("failed to render page", "err", err)
		}
	})

	// GET /teams lists all teams.
	handler.HandleFunc("GET /teams", func(w http.ResponseWriter, r *http.Request) {
		isAdmin := game.checkForAdmin(w, r)

		var teamList []htm.Fragment
		for _, team := range game.Teams {
			var lines []htm.Fragment
			lines = append(lines, bootstrap.CardTitle(team.DisplayName))
			lines = append(lines, bootstrap.CardTitle(fmt.Sprintf("IP: %s", team.IP())))
			if isAdmin {
				lines = append(lines, bootstrap.CardTitle(fmt.Sprintf("Join token: %s", team.JoinToken)))
			}
			teamList = append(teamList, html.Div(bootstrap.Card(lines...)))
		}

		page := game.publicPageLayout("Teams", teamList...)

		if err := htm.Render(r.Context(), w, page); err != nil {
			slog.Error("failed to render page", "err", err)
		}
	})

	// GET /connect/{instance} provides a WebSSH terminal to the instance.
	handler.HandleFunc("GET /connect/{instance}", func(w http.ResponseWriter, r *http.Request) {
		// TODO(joshua): Allow teams to connect to their own instances.
		if !game.checkForAdmin(w, r) {
			return
		}

		instanceId := r.PathValue("instance")

		if _, err := game.instanceFromName(instanceId); err != nil {
			if err := htm.Render(r.Context(), w, game.publicPageError(err)); err != nil {
				slog.Error("failed to render page", "err", err)
			}
			return
		}

		page := game.publicPageLayout("Connect",
			htm.Group{
				xtermjs.XTERM_CSS,
				xtermjs.XTERM_JS,
				xtermjs.XTERM_ADDON_FIT,
				bootstrap.Button(bootstrap.ButtonColorDark, html.Text("Toggle Fill Screen"), html.Id("fillScreen")),
				html.Div(html.Id("terminal"), htm.Attr("data-connect", "/api/connect/"+instanceId)),
				xtermjs.SSH_CSS,
				xtermjs.SSH_JS,
			},
		)

		if err := htm.Render(r.Context(), w, page); err != nil {
			slog.Error("failed to render page", "err", err)
		}
	})

	// /api/connect/{instance} provides a WebSocket connection to the instance.
	handler.HandleFunc("/api/connect/{instance}", func(w http.ResponseWriter, r *http.Request) {
		if !game.checkForAdmin(w, r) {
			return
		}

		instanceName := r.PathValue("instance")

		instance, err := game.instanceFromName(instanceName)
		if err != nil {
			slog.Error("failed to get instance", "err", err)
			http.Error(w, "instance not found", http.StatusNotFound)
			return
		}

		// Upgrade the connection to a WebSocket.
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			slog.Error("failed to upgrade connection", "err", err)
			return
		}

		if err := instance.WebSSHHandler(conn); err != nil {
			slog.Error("failed to handle WebSSH", "err", err)
		}
	})

	handler.HandleFunc("GET /events", func(w http.ResponseWriter, r *http.Request) {
		if !game.checkForAdmin(w, r) {
			return
		}

		events := game.GetEvents()

		var eventList []htm.Fragment

		for _, event := range events {
			eventList = append(eventList, html.Div(
				bootstrap.Card(
					bootstrap.CardTitle(event),
					html.Form(
						html.FormTarget("POST", "/api/event"),
						html.HiddenFormField(html.NewId(), "name", event),
						bootstrap.SubmitButton(event, bootstrap.ButtonColorPrimary),
					),
				),
			))
		}

		page := game.publicPageLayout("Events", eventList...)

		if err := htm.Render(r.Context(), w, page); err != nil {
			slog.Error("failed to render page", "err", err)
		}
	})

	// POST /event runs an event by name.
	handler.HandleFunc("POST /api/event", func(w http.ResponseWriter, r *http.Request) {
		if !game.checkForAdmin(w, r) {
			return
		}

		name := r.FormValue("name")

		if err := game.RunEvent(r.Context(), name); err != nil {
			slog.Error("failed to run event", "err", err)
			if err := htm.Render(r.Context(), w, game.publicPageError(err)); err != nil {
				slog.Error("failed to render page", "err", err)
			}
			return
		}

		http.Redirect(w, r, "/", http.StatusFound)
	})

	// GET /devices lists all devices and their WireGuard configuration and a button to add a new device.
	handler.HandleFunc("GET /devices", func(w http.ResponseWriter, r *http.Request) {
		// TODO(joshua): Allow teams to add their own instances.
		if !game.checkForAdmin(w, r) {
			return
		}

		devices := game.GetDevices()

		var deviceList []htm.Fragment

		for _, device := range devices {
			deviceList = append(deviceList, html.Div(
				bootstrap.Card(
					bootstrap.CardTitle(device.Name),
					html.Div(html.Strong(html.Text("IP Address:")), html.Textf("%s", device.IP)),
					html.Div(
						html.Link(device.ConfigUrl, html.Textf("Wireguard client config")),
					),
					// html.Form(
					// 	html.FormTarget("DELETE", fmt.Sprintf("/api/device/%s", device.IP)),
					// 	bootstrap.SubmitButton("Delete", bootstrap.ButtonColorDanger),
					// ),
				),
			))
		}

		teamNames := []string{}
		for _, team := range game.Teams {
			teamNames = append(teamNames, team.DisplayName)
		}

		page := game.publicPageLayout("Devices",
			htm.Group(deviceList),
			html.Form(
				html.FormTarget("POST", "/api/device"),
				bootstrap.FormField("Name", "name", html.FormOptions{Kind: html.FormFieldText, Required: true, Value: "", Placeholder: "Device Name"}),
				bootstrap.FormField("Team", "team", html.FormOptions{Kind: html.FormFieldSelect, Required: true, Value: "", Placeholder: "Team", Options: teamNames}),
				bootstrap.SubmitButton("Add Device", bootstrap.ButtonColorPrimary),
			),
		)

		if err := htm.Render(r.Context(), w, page); err != nil {
			slog.Error("failed to render page", "err", err)
		}
	})

	// POST /api/device adds a new device.
	handler.HandleFunc("POST /api/device", func(w http.ResponseWriter, r *http.Request) {
		if !game.checkForAdmin(w, r) {
			return
		}

		name := r.FormValue("name")
		if name == "" {
			game.renderError(w, r, fmt.Errorf("name is required"))
			return
		}

		userIDStr := r.FormValue("userID")
		if userIDStr == "" {
			game.renderError(w, r, fmt.Errorf("userID is required"))
			return
		}

		userID, err := strconv.Atoi(userIDStr)
		if err != nil {
			game.renderError(w, r, fmt.Errorf("userID must be an integer"))
			return
		}

		if err := game.AddDevice(name, userID); err != nil {
			slog.Error("failed to add device", "err", err)
			game.renderError(w, r, err)
			return
		}

		http.Redirect(w, r, "/devices", http.StatusFound)
	})

	handler.HandleFunc("GET /register", func(w http.ResponseWriter, r *http.Request) {
		page := game.publicPageLayout("Register",
			html.Form(
				html.FormTarget("POST", "/register"),
				bootstrap.FormField("Team token", "teamToken", html.FormOptions{Kind: html.FormFieldText, Required: true, Value: "", Placeholder: "Team token"}),
				bootstrap.FormField("Username", "username", html.FormOptions{Kind: html.FormFieldText, Required: true, Value: "", Placeholder: "Username"}),
				bootstrap.FormField("Password", "password", html.FormOptions{Kind: html.FormFieldPassword, Required: true, Value: "", Placeholder: "Password"}),
				bootstrap.SubmitButton("Register", bootstrap.ButtonColorPrimary),
			),
		)

		if err := htm.Render(r.Context(), w, page); err != nil {
			slog.Error("failed to render page", "err", err)
		}
	})

	// POST /register adds a new user.
	handler.HandleFunc("POST /register", func(w http.ResponseWriter, r *http.Request) {
		teamToken := r.FormValue("teamToken")
		if teamToken == "" {
			game.renderError(w, r, fmt.Errorf("teamToken is required"))
			return
		}

		username := r.FormValue("username")
		if username == "" {
			game.renderError(w, r, fmt.Errorf("username is required"))
			return
		}

		password := r.FormValue("password")
		if password == "" {
			game.renderError(w, r, fmt.Errorf("password is required"))
			return
		}

		teamID := -1
		for _, team := range game.Teams {
			if team.JoinToken == teamToken {
				teamID = team.ID
			}
		}

		if teamID == -1 {
			game.renderError(w, r, fmt.Errorf("invalid team token"))
			return
		}

		_, err := game.Persist.GetUserByUsername(username)
		if err == nil {
			game.renderError(w, r, fmt.Errorf("username taken"))
			return
		}

		passwordHash, err := HashPassword(password)
		if err != nil {
			game.renderError(w, r, fmt.Errorf("failed to hash password: %v", err))
			return
		}

		user := User{
			TeamID:       teamID,
			Username:     username,
			PasswordHash: passwordHash,
		}
		_, err = game.Persist.InsertUser(&user)
		if err != nil {
			game.renderError(w, r, fmt.Errorf("failed to insert user: %v", err))
			return
		}

		http.Redirect(w, r, "/login", http.StatusFound)
	})

	handler.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
		page := game.publicPageLayout("Log in",
			html.Form(
				html.FormTarget("POST", "/login"),
				bootstrap.FormField("Username", "username", html.FormOptions{Kind: html.FormFieldText, Required: true, Value: "", Placeholder: "Username"}),
				bootstrap.FormField("Password", "password", html.FormOptions{Kind: html.FormFieldPassword, Required: true, Value: "", Placeholder: "Password"}),
				bootstrap.SubmitButton("Log in", bootstrap.ButtonColorPrimary),
			),
		)

		if err := htm.Render(r.Context(), w, page); err != nil {
			slog.Error("failed to render page", "err", err)
		}
	})

	// POST /login logs in a user.
	handler.HandleFunc("POST /login", func(w http.ResponseWriter, r *http.Request) {
		username := r.FormValue("username")
		if username == "" {
			game.renderError(w, r, fmt.Errorf("username is required"))
			return
		}

		password := r.FormValue("password")
		if password == "" {
			game.renderError(w, r, fmt.Errorf("password is required"))
			return
		}

		user, err := game.Persist.GetUserByUsername(username)
		if err != nil || !user.VerifyPassword(password) {
			game.renderError(w, r, fmt.Errorf("incorrect username or password"))
			return
		}

		token, err := GenerateRandomString(64)
		if err != nil {
			game.renderError(w, r, fmt.Errorf("failed to generate session token"))
			return
		}

		session := Session{
			Token:     token,
			UserID:    user.ID,
			ExpiresAt: time.Now().Add(SessionValidDuration),
		}
		_, err = game.Persist.InsertSession(&session)
		if err != nil {
			game.renderError(w, r, fmt.Errorf("failed to create session: %v", err))
		}

		cookie := http.Cookie{Name: "session", Value: session.Token, Expires: session.ExpiresAt}
		http.SetCookie(w, &cookie)

		http.Redirect(w, r, "/", http.StatusFound)
	})

	handler.HandleFunc("GET /profile", func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("session")
		if err != nil {
			game.renderError(w, r, fmt.Errorf("unauthorized"))
			return
		}

		session, err := game.Persist.GetSessionByToken(cookie.Value)
		if err != nil {
			game.renderError(w, r, fmt.Errorf("unauthorized"))
			return
		}

		user, err := game.Persist.GetUser(session.UserID)
		if err != nil {
			game.renderError(w, r, fmt.Errorf("failed to get user by id"))
			return
		}

		var team *Team
		for _, candidateTeam := range game.Teams {
			if candidateTeam.ID == user.TeamID {
				team = candidateTeam
			}
		}

		if team == nil {
			game.renderError(w, r, fmt.Errorf("failed to get team by id"))
			return
		}

		page := game.publicPageLayout("Profile",
			html.Div(
				html.Div(html.Textf("Username: %s", user.Username)),
				html.Div(html.Textf("Team: %s", team.DisplayName)),
			),
		)

		if err := htm.Render(r.Context(), w, page); err != nil {
			slog.Error("failed to render page", "err", err)
		}
	})

	// // DELETE /api/device/{ip} deletes a device.
	// handler.HandleFunc("DELETE /api/device/{ip}", func(w http.ResponseWriter, r *http.Request) {
	// 	if !game.checkForAdmin(w, r) {
	// 		return
	// 	}

	// 	ip := r.PathValue("ip")

	// 	if err := game.RemoveDevice(ip); err != nil {
	// 		slog.Error("failed to remove device", "err", err)
	// 		if err := htm.Render(r.Context(), w, game.publicPageError(err)); err != nil {
	// 			slog.Error("failed to render page", "err", err)
	// 		}
	// 		return
	// 	}

	// 	http.Redirect(w, r, "/devices", http.StatusFound)
	// })

	// GET /config lists the current YAML configuration and provides a button to download it.
	handler.HandleFunc("GET /config", func(w http.ResponseWriter, r *http.Request) {
		if !game.checkForAdmin(w, r) {
			return
		}

		config, err := yaml.Marshal(&game.Config)
		if err != nil {
			slog.Error("failed to marshal config", "err", err)
			http.Error(w, "failed to marshal config", http.StatusInternalServerError)
			return
		}

		if err := htm.Render(r.Context(), w, game.publicPageLayout("Config", html.Pre(html.Code(html.Textf("%s", config))))); err != nil {
			slog.Error("failed to render page", "err", err)
		}
	})

	// GET /api/config downloads the current YAML configuration.
	handler.HandleFunc("GET /api/config", func(w http.ResponseWriter, r *http.Request) {
		if !game.checkForAdmin(w, r) {
			return
		}

		w.Header().Set("Content-Type", "application/yaml")

		if err := yaml.NewEncoder(w).Encode(&game.Config); err != nil {
			slog.Error("failed to encode config", "err", err)
			http.Error(w, "failed to encode config", http.StatusInternalServerError)
			return
		}
	})

	// GET /scoreboard lists the scoreboard for the overall state.
	handler.HandleFunc("GET /scoreboard", func(w http.ResponseWriter, r *http.Request) {
		page := game.renderScoreboard()
		if page == nil {
			page = game.publicPageError(fmt.Errorf("game has not started"))
		} else {
			page = game.publicPageLayout("Scoreboard", page)
		}

		if err := htm.Render(r.Context(), w, page); err != nil {
			slog.Error("failed to render page", "err", err)
		}
	})

	// GET /api/scoreboard returns the scoreboard for the overall state.
	handler.HandleFunc("GET /api/scoreboard", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		game.scoreboardMtx.RLock()
		defer game.scoreboardMtx.RUnlock()

		if err := json.NewEncoder(w).Encode(game.OverallState); err != nil {
			slog.Error("failed to encode scoreboard", "err", err)
		}
	})

	// GET /api/scoreboard/{tick} returns the scoreboard for a specific tick.
	handler.HandleFunc("GET /api/scoreboard/{tick}", func(w http.ResponseWriter, r *http.Request) {
		tickStr := r.PathValue("tick")

		tick, err := strconv.Atoi(tickStr)
		if err != nil {
			http.Error(w, "invalid tick", http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")

		game.scoreboardMtx.RLock()
		defer game.scoreboardMtx.RUnlock()

		if tick > len(game.Ticks) {
			http.Error(w, "tick not found", http.StatusNotFound)
			return
		}

		if err := json.NewEncoder(w).Encode(game.Ticks[tick-1]); err != nil {
			slog.Error("failed to encode scoreboard", "err", err)
		}
	})

	for path, pageInfo := range game.Config.Pages {
		if path == "/" {
			continue
		}

		handler.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
			body, err := game.renderPage(r.Context(), path)
			if err != nil {
				slog.Error("failed to render page", "err", err)
				if err := htm.Render(r.Context(), w, game.publicPageError(err)); err != nil {
					slog.Error("failed to render page", "err", err)
				}
				return
			}

			page := game.publicPageLayout(pageInfo.Title, body)

			if err := htm.Render(r.Context(), w, page); err != nil {
				slog.Error("failed to render page", "err", err)
			}
		})
	}

	// Router is allowed to be public since it uses an API key to lookup a configuration.
	game.Router.RegisterMux(handler)

	listenAddr := fmt.Sprintf("%s:%d", game.ListenIP, game.PublicPort)

	game.publicServer = &http.Server{
		Addr:    listenAddr,
		Handler: handler,
	}

	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("failed to listen: %w", err)
	}

	slog.Info("public server listening", "url", " "+game.FrontendUrl())

	go func() {
		if err := game.publicServer.Serve(listener); err != nil {
			slog.Error("failed to start server", "err", err)
		}
	}()

	return nil
}
