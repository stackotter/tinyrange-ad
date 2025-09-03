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
	"strings"
	"time"

	"github.com/gomarkdown/markdown"
	htmlMd "github.com/gomarkdown/markdown/html"
	"github.com/gorilla/websocket"
	"github.com/tinyrange/ad/pkg/htm"
	"github.com/tinyrange/ad/pkg/htm/bootstrap"
	"github.com/tinyrange/ad/pkg/htm/html"
	"github.com/tinyrange/ad/pkg/htm/htmx"
	"github.com/tinyrange/ad/pkg/htm/xtermjs"
	"golang.org/x/crypto/ssh"
	"gopkg.in/yaml.v3"
)

func (game *AttackDefenseGame) requireAuthentication(w http.ResponseWriter, r *http.Request) (User, Team, error) {
	cookie, err := r.Cookie("session")
	if err != nil {
		return User{}, Team{}, fmt.Errorf("unauthorized")
	}

	session, err := game.Persist.GetSessionByToken(cookie.Value)
	if err != nil {
		return User{}, Team{}, fmt.Errorf("unauthorized")
	}

	user, err := game.Persist.GetUser(session.UserID)
	if err != nil {
		return User{}, Team{}, fmt.Errorf("failed to get user by id")
	}

	var team *Team
	for _, candidateTeam := range game.Teams {
		if candidateTeam.ID == user.TeamID {
			team = candidateTeam
		}
	}

	if team == nil {
		return User{}, Team{}, fmt.Errorf("failed to get team by id")
	}

	return user, *team, nil
}

func (game *AttackDefenseGame) isAdmin(user User) bool {
	team, err := game.Persist.GetTeam(user.TeamID)
	if err != nil {
		slog.Error("failed to get user team", "err", err)
		return false
	}
	return team.IsAdmin()
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
		diff := a.Position - b.Position
		if diff == 0 {
			return strings.Compare(a.Name, b.Name)
		} else {
			return diff
		}
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

func (game *AttackDefenseGame) publicPageError(err error, user *User) htm.Fragment {
	return game.publicPageLayout("Error", user, bootstrap.Alert(bootstrap.AlertColorDanger, htm.Text(err.Error())))
}

func (game *AttackDefenseGame) publicPageLayout(title string, user *User, body ...htm.Fragment) htm.Fragment {
	var navitems []htm.Fragment
	navitems = append(navitems,
		bootstrap.NavbarLink("/scoreboard", html.Text("Scoreboard")),
	)

	if user != nil && game.isAdmin(*user) {
		navitems = append(navitems,
			bootstrap.NavbarLink("/game", html.Text("Game")),
			bootstrap.NavbarLink("/events", html.Text("Events")),
			bootstrap.NavbarLink("/config", html.Text("Config")),
		)
	}

	if user == nil {
		navitems = append(navitems,
			bootstrap.NavbarLink("/register", html.Text("Register")),
			bootstrap.NavbarLink("/login", html.Text("Log in")),
		)
	} else {
		navitems = append(navitems,
			bootstrap.NavbarLink("/teams", html.Text("Teams")),
			bootstrap.NavbarLink("/devices", html.Text("Devices")),
			bootstrap.NavbarLink("/instances", html.Text("Instances")),
			bootstrap.NavbarLink("/team", html.Text("Team")),
			bootstrap.NavbarLink("/profile", html.Text("Profile")),
			bootstrap.NavbarLink("/logout", html.Text("Log out")),
		)
	}

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
				navitems...,
			),
			html.Div(bootstrap.Container, html.P(body...)),
		),
	)
}

func (game *AttackDefenseGame) renderError(w http.ResponseWriter, r *http.Request, err error, user *User) {
	if err := htm.Render(r.Context(), w, game.publicPageError(err, user)); err != nil {
		slog.Error("failed to render page", "err", err)
	}
}

func (game *AttackDefenseGame) route(
	handler func(w http.ResponseWriter, r *http.Request, user *User, team *Team) error,
) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		user, team, err := game.requireAuthentication(w, r)
		userPtr := &user
		teamPtr := &team
		if err != nil {
			userPtr = nil
			teamPtr = nil
		}
		err = handler(w, r, userPtr, teamPtr)
		if err != nil {
			game.renderError(w, r, err, userPtr)
		}
	}
}

func (game *AttackDefenseGame) authenticatedRoute(
	handler func(w http.ResponseWriter, r *http.Request, user User, team Team) error,
) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		user, team, err := game.requireAuthentication(w, r)
		if err != nil {
			game.renderError(w, r, err, nil)
			return
		}
		err = handler(w, r, user, team)
		if err != nil {
			game.renderError(w, r, err, &user)
		}
	}
}

func (game *AttackDefenseGame) adminRoute(
	handler func(w http.ResponseWriter, r *http.Request, user User, team Team) error,
) func(w http.ResponseWriter, r *http.Request) {
	return game.authenticatedRoute(
		func(w http.ResponseWriter, r *http.Request, user User, team Team) error {
			if !game.isAdmin(user) {
				return fmt.Errorf("unauthorized")
			}
			return handler(w, r, user, team)
		},
	)
}

func (game *AttackDefenseGame) startPublicServer() error {
	handler := http.NewServeMux()

	handler.HandleFunc("/", game.route(func(w http.ResponseWriter, r *http.Request, user *User, team *Team) error {
		body, err := game.renderPage(r.Context(), "/")
		if err != nil {
			return err
		}

		page := game.publicPageLayout("Home", user, body)

		err = htm.Render(r.Context(), w, page)
		return err
	}))

	// GET /instances lists all running TinyRange instances for the logged in user's team and provides a button to SSH via WebSSH.
	handler.HandleFunc("GET /instances", game.authenticatedRoute(func(w http.ResponseWriter, r *http.Request, user User, team Team) error {
		var instances []TinyRangeInstance
		if game.isAdmin(user) {
			instances = game.getInstances()
		} else {
			if game.teamInstance(team.ID) != nil {
				instances = append(instances, *game.teamInstance(team.ID))
			}
			if game.socInstance(team.ID) != nil {
				instances = append(instances, *game.socInstance(team.ID))
			}
			if game.botInstance(team.ID) != nil {
				instances = append(instances, *game.botInstance(team.ID))
			}
		}

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

		if len(instances) == 0 {
			instanceList = append(instanceList, html.Text("No instances"))
		}

		page := game.publicPageLayout("Instances", &user, instanceList...)

		err := htm.Render(r.Context(), w, page)
		return err
	}))

	// GET /teams lists all teams.
	handler.HandleFunc("GET /game", game.adminRoute(func(w http.ResponseWriter, r *http.Request, user User, team Team) error {
		var content []htm.Fragment

		runningState := game.RunningState.Load()
		if runningState == RunningStateStarted {
			content = append(content,
				html.P(htm.Text("Game is running")),
				html.P(html.Textf("Tick %d out of %d", game.CurrentTick, game.TotalTicks())),
			)
		} else if runningState == RunningStateStarting {
			content = append(content,
				html.P(htm.Text("Game is starting")),
			)
		} else {
			content = append(content,
				html.Form(
					html.FormTarget("POST", "/api/game/start"),
					bootstrap.SubmitButton("Start", bootstrap.ButtonColorPrimary),
				),
			)
			if game.Error != nil {
				content = append(content,
					html.P(html.Textf("Game failed to start: %v", game.Error)),
				)
			}
		}

		page := game.publicPageLayout("Game", &user, content...)

		err := htm.Render(r.Context(), w, page)
		return err
	}))

	// GET /teams lists all teams.
	handler.HandleFunc("POST /api/game/start", game.adminRoute(func(w http.ResponseWriter, r *http.Request, user User, team Team) error {
		go func() {
			if err := game.Start(); err != nil {
				slog.Error("Failed to start game", "err", err)
				game.Error = err
			}
		}()

		http.Redirect(w, r, "/game", http.StatusFound)
		return nil
	}))

	// GET /teams lists all teams.
	handler.HandleFunc("GET /teams", game.authenticatedRoute(func(w http.ResponseWriter, r *http.Request, user User, team Team) error {
		isAdmin := game.isAdmin(user)

		var teamList []htm.Fragment
		var teams []*Team
		teams = slices.AppendSeq(teams, maps.Values(game.Teams))
		slices.SortFunc(teams, func(i *Team, j *Team) int {
			if i.ID < j.ID {
				return -1
			} else if i.ID > j.ID {
				return 1
			} else {
				return 0
			}
		})
		for _, team := range teams {
			if team.IsAdmin() && !game.isAdmin(user) {
				continue
			}
			var lines []htm.Fragment
			lines = append(lines, bootstrap.CardTitle(team.DisplayName))
			if !team.IsAdmin() {
				lines = append(lines, bootstrap.CardTitle(fmt.Sprintf("IP: %s", team.IP())))
			}
			if isAdmin {
				lines = append(lines, bootstrap.CardTitle(fmt.Sprintf("Join token: %s", team.JoinToken)))
			}
			teamList = append(teamList, html.Div(bootstrap.Card(lines...)))
		}

		var content []htm.Fragment
		content = append(content, html.Div(teamList...))

		if isAdmin && game.RunningState.Load() == RunningStateStopped {
			content = append(content,
				html.P(
					html.H2(htm.Text("Create team")),
					html.Form(
						html.FormTarget("POST", "/api/team"),
						bootstrap.FormField("Name", "name", html.FormOptions{Kind: html.FormFieldText, Required: true, Value: "", Placeholder: "Name"}),
						bootstrap.SubmitButton("Create team", bootstrap.ButtonColorPrimary),
					),
				),
			)
		}

		page := game.publicPageLayout("Teams", &user, content...)

		err := htm.Render(r.Context(), w, page)
		return err
	}))

	// POST /api/team adds a new team.
	handler.HandleFunc("POST /api/team", game.adminRoute(func(w http.ResponseWriter, r *http.Request, user User, team Team) error {
		if game.RunningState.Load() != RunningStateStopped {
			return fmt.Errorf("game already started")
		}

		name := r.FormValue("name")
		if name == "" {
			return fmt.Errorf("name is required")
		}

		if err := game.AddTeam(name); err != nil {
			slog.Error("failed to add team", "err", err)
			return err
		}

		http.Redirect(w, r, "/teams", http.StatusFound)
		return nil
	}))

	// GET /connect/{instance} provides a WebSSH terminal to the instance.
	handler.HandleFunc("GET /connect/{instance}", game.authenticatedRoute(func(w http.ResponseWriter, r *http.Request, user User, team Team) error {
		instanceId := r.PathValue("instance")

		instance, err := game.instanceFromName(instanceId)
		if err != nil {
			return err
		}

		if !game.isAdmin(user) && instance.TeamID() != team.ID {
			return fmt.Errorf("unauthorized")
		}

		page := game.publicPageLayout("Connect", &user,
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

		err = htm.Render(r.Context(), w, page)
		return err
	}))

	// /api/connect/{instance} provides a WebSocket connection to the instance.
	handler.HandleFunc("/api/connect/{instance}", game.authenticatedRoute(func(w http.ResponseWriter, r *http.Request, user User, team Team) error {
		instanceName := r.PathValue("instance")

		instance, err := game.instanceFromName(instanceName)
		if err != nil {
			slog.Error("failed to get instance", "err", err)
			http.Error(w, "instance not found", http.StatusNotFound)
			return nil
		}

		if !game.isAdmin(user) && instance.TeamID() != team.ID {
			slog.Error("unauthorized", "err", err)
			return fmt.Errorf("unauthorized")
		}

		// Upgrade the connection to a WebSocket.
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			slog.Error("failed to upgrade connection", "err", err)
			return nil
		}

		if err := instance.WebSSHHandler(conn); err != nil {
			slog.Error("failed to handle WebSSH", "err", err)
			return nil
		}

		return nil
	}))

	handler.HandleFunc("GET /events", game.adminRoute(func(w http.ResponseWriter, r *http.Request, user User, team Team) error {
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

		page := game.publicPageLayout("Events", &user, eventList...)

		err := htm.Render(r.Context(), w, page)
		return err
	}))

	// POST /event runs an event by name.
	handler.HandleFunc("POST /api/event", game.adminRoute(func(w http.ResponseWriter, r *http.Request, user User, team Team) error {
		name := r.FormValue("name")

		if err := game.RunEvent(r.Context(), name); err != nil {
			slog.Error("failed to run event", "err", err)
			return err
		}

		http.Redirect(w, r, "/", http.StatusFound)
		return nil
	}))

	// GET /devices lists all devices and their WireGuard configuration and a button to add a new device.
	handler.HandleFunc("GET /devices", game.authenticatedRoute(func(w http.ResponseWriter, r *http.Request, user User, team Team) error {
		devices := game.GetDevices()

		var deviceList []htm.Fragment

		for _, device := range devices {
			if device.UserID != user.ID {
				continue
			}
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

		page := game.publicPageLayout("Devices", &user,
			html.Div(deviceList...),
			html.P(
				html.H2(htm.Text("Add device")),
				html.Form(
					html.FormTarget("POST", "/api/device"),
					bootstrap.FormField("Name", "name", html.FormOptions{Kind: html.FormFieldText, Required: true, Value: "", Placeholder: "Device Name"}),
					bootstrap.SubmitButton("Add device", bootstrap.ButtonColorPrimary),
				),
			),
		)

		err := htm.Render(r.Context(), w, page)
		return err
	}))

	// POST /api/device adds a new device.
	handler.HandleFunc("POST /api/device", game.authenticatedRoute(func(w http.ResponseWriter, r *http.Request, user User, team Team) error {
		name := r.FormValue("name")
		if name == "" {
			return fmt.Errorf("name is required")
		}

		if err := game.AddDevice(name, user.ID); err != nil {
			slog.Error("failed to add device", "err", err)
			return err
		}

		http.Redirect(w, r, "/devices", http.StatusFound)
		return nil
	}))

	handler.HandleFunc("GET /register", game.route(func(w http.ResponseWriter, r *http.Request, user *User, team *Team) error {
		if user != nil {
			http.Redirect(w, r, "/", http.StatusFound)
			return nil
		}

		page := game.publicPageLayout("Register", user,
			html.H1(html.Text("Register")),
			html.Form(
				html.FormTarget("POST", "/register"),
				bootstrap.FormField("Team token", "teamToken", html.FormOptions{Kind: html.FormFieldText, Required: true, Value: "", Placeholder: "Team token"}),
				bootstrap.FormField("Username", "username", html.FormOptions{Kind: html.FormFieldText, Required: true, Value: "", Placeholder: "Username"}),
				bootstrap.FormField("Password", "password", html.FormOptions{Kind: html.FormFieldPassword, Required: true, Value: "", Placeholder: "Password"}),
				bootstrap.SubmitButton("Register", bootstrap.ButtonColorPrimary),
			),
		)

		err := htm.Render(r.Context(), w, page)
		return err
	}))

	// POST /register adds a new user.
	handler.HandleFunc("POST /register", game.route(func(w http.ResponseWriter, r *http.Request, user *User, team *Team) error {
		if user != nil {
			http.Redirect(w, r, "/", http.StatusFound)
			return nil
		}

		teamToken := r.FormValue("teamToken")
		if teamToken == "" {
			return fmt.Errorf("teamToken is required")
		}

		username := r.FormValue("username")
		if username == "" {
			return fmt.Errorf("username is required")
		}

		password := r.FormValue("password")
		if password == "" {
			return fmt.Errorf("password is required")
		}

		teamID := -1
		for _, team := range game.Teams {
			if team.JoinToken == teamToken {
				teamID = team.ID
			}
		}

		if teamID == -1 {
			return fmt.Errorf("invalid team token")
		}

		_, err := game.Persist.GetUserByUsername(username)
		if err == nil {
			return fmt.Errorf("username taken")
		}

		passwordHash, err := HashPassword(password)
		if err != nil {
			return fmt.Errorf("failed to hash password: %v", err)
		}

		newUser := User{
			TeamID:       teamID,
			Username:     username,
			PasswordHash: passwordHash,
		}
		_, err = game.Persist.InsertUser(&newUser)
		if err != nil {
			return fmt.Errorf("failed to insert user: %v", err)
		}

		http.Redirect(w, r, "/login", http.StatusFound)
		return nil
	}))

	handler.HandleFunc("GET /login", game.route(func(w http.ResponseWriter, r *http.Request, user *User, team *Team) error {
		if user != nil {
			http.Redirect(w, r, "/", http.StatusFound)
			return nil
		}

		page := game.publicPageLayout("Log in", user,
			html.H1(html.Text("Log in")),
			html.Form(
				html.FormTarget("POST", "/login"),
				bootstrap.FormField("Username", "username", html.FormOptions{Kind: html.FormFieldText, Required: true, Value: "", Placeholder: "Username"}),
				bootstrap.FormField("Password", "password", html.FormOptions{Kind: html.FormFieldPassword, Required: true, Value: "", Placeholder: "Password"}),
				bootstrap.SubmitButton("Log in", bootstrap.ButtonColorPrimary),
			),
		)

		err := htm.Render(r.Context(), w, page)
		return err
	}))

	// TODO(stackotter): Protect against CSRF (in all applicable routes)
	handler.HandleFunc("GET /logout", game.route(func(w http.ResponseWriter, r *http.Request, user *User, team *Team) error {
		cookie, err := r.Cookie("session")
		if err != nil {
			http.Redirect(w, r, "/", http.StatusFound)
			return nil
		}

		err = game.Persist.DeleteSessionByToken(cookie.Value)
		if err != nil {
			slog.Error("failed to delete session by token", "err", err)
			return fmt.Errorf("failed to delete session by token")
		}

		blankCookie := http.Cookie{Name: "session", Value: "", Expires: time.Unix(0, 0)}
		http.SetCookie(w, &blankCookie)

		http.Redirect(w, r, "/", http.StatusFound)
		return nil
	}))

	// POST /login logs in a user.
	handler.HandleFunc("POST /login", game.route(func(w http.ResponseWriter, r *http.Request, sessionUser *User, team *Team) error {
		if sessionUser != nil {
			http.Redirect(w, r, "/", http.StatusFound)
			return nil
		}

		username := r.FormValue("username")
		if username == "" {
			return fmt.Errorf("username is required")
		}

		password := r.FormValue("password")
		if password == "" {
			return fmt.Errorf("password is required")
		}

		user, err := game.Persist.GetUserByUsername(username)
		if err != nil || !user.VerifyPassword(password) {
			return fmt.Errorf("incorrect username or password")
		}

		token, err := GenerateRandomString(64)
		if err != nil {
			return fmt.Errorf("failed to generate session token")
		}

		session := Session{
			Token:     token,
			UserID:    user.ID,
			ExpiresAt: time.Now().Add(SessionValidDuration),
		}
		_, err = game.Persist.InsertSession(&session)
		if err != nil {
			return fmt.Errorf("failed to create session: %v", err)
		}

		cookie := http.Cookie{Name: "session", Value: session.Token, Expires: session.ExpiresAt}
		http.SetCookie(w, &cookie)

		http.Redirect(w, r, "/", http.StatusFound)
		return nil
	}))

	handler.HandleFunc("GET /profile", game.authenticatedRoute(func(w http.ResponseWriter, r *http.Request, user User, team Team) error {
		page := game.publicPageLayout("Profile", &user,
			html.Div(
				html.Div(html.Textf("Username: %s", user.Username)),
				html.Div(html.Textf("Team: %s", team.DisplayName)),
			),
		)

		err := htm.Render(r.Context(), w, page)
		return err
	}))

	handler.HandleFunc("GET /team", game.authenticatedRoute(func(w http.ResponseWriter, r *http.Request, user User, team Team) error {
		if game.RunningState.Load() != RunningStateStarted {
			return fmt.Errorf("game not started yet")
		}

		secureConfig, err := game.GetSSHConfig(team.ID)
		if err != nil {
			slog.Error("failed to get secure config", "err", err)
			return err
		}

		publicKey, _, _, _, err := ssh.ParseAuthorizedKey([]byte(secureConfig.PublicKey))
		if err != nil {
			slog.Error("failed to parse public key", "err", err)
			return err
		}

		content := bootstrap.Card(
			bootstrap.CardTitle(team.DisplayName),
			html.P(html.Strong(htm.Text("SSH Command: ")), html.Code(html.Textf("ssh -p 2222 root@%s", team.IP()))),
			bootstrap.Table(
				nil,
				[]htm.Group{
					{htm.Text("IP"), html.Code(html.Textf("%s", team.IP()))},
					{htm.Text("Port"), html.Code(html.Textf("%d", 2222))},
					{htm.Text("Username"), html.Code(html.Textf("%s", "root"))},
					{htm.Text("Password"), html.Code(html.Textf("%s", secureConfig.Password))},
					{htm.Text("Fingerprint"), html.Code(html.Textf("%s", ssh.FingerprintSHA256(publicKey)))},
				},
			),
		)

		page := game.publicPageLayout("Team", &user,
			content,
		)

		err = htm.Render(r.Context(), w, page)
		return err
	}))

	// // DELETE /api/device/{ip} deletes a device.
	// handler.HandleFunc("DELETE /api/device/{ip}", func(w http.ResponseWriter, r *http.Request) {
	// 	if !game.checkForAdmin(w, r) {
	// 		return
	// 	}

	// 	ip := r.PathValue("ip")

	// 	if err := game.RemoveDevice(ip); err != nil {
	// 		slog.Error("failed to remove device", "err", err)
	// 		if err := htm.Render(r.Context(), w, game.publicPageError(err, user)); err != nil {
	// 			slog.Error("failed to render page", "err", err)
	// 		}
	// 		return
	// 	}

	// 	http.Redirect(w, r, "/devices", http.StatusFound)
	// })

	// GET /config lists the current YAML configuration and provides a button to download it.
	handler.HandleFunc("GET /config", game.adminRoute(func(w http.ResponseWriter, r *http.Request, user User, team Team) error {
		config, err := yaml.Marshal(&game.Config)
		if err != nil {
			slog.Error("failed to marshal config", "err", err)
			http.Error(w, "failed to marshal config", http.StatusInternalServerError)
			return nil
		}

		err = htm.Render(r.Context(), w,
			game.publicPageLayout("Config", &user,
				html.Pre(html.Code(html.Textf("%s", config))),
			),
		)
		return err
	}))

	// GET /api/config downloads the current YAML configuration.
	handler.HandleFunc("GET /api/config", game.adminRoute(func(w http.ResponseWriter, r *http.Request, user User, team Team) error {
		w.Header().Set("Content-Type", "application/yaml")

		if err := yaml.NewEncoder(w).Encode(&game.Config); err != nil {
			slog.Error("failed to encode config", "err", err)
			http.Error(w, "failed to encode config", http.StatusInternalServerError)
			return nil
		}

		return nil
	}))

	// GET /scoreboard lists the scoreboard for the overall state.
	handler.HandleFunc("GET /scoreboard", game.route(func(w http.ResponseWriter, r *http.Request, user *User, team *Team) error {
		page := game.renderScoreboard()
		if page == nil {
			return fmt.Errorf("game has not started")
		} else {
			page = game.publicPageLayout("Scoreboard", user, page)
		}

		err := htm.Render(r.Context(), w, page)
		return err
	}))

	// GET /api/scoreboard returns the scoreboard for the overall state.
	handler.HandleFunc("GET /api/scoreboard", game.route(func(w http.ResponseWriter, r *http.Request, user *User, team *Team) error {
		w.Header().Set("Content-Type", "application/json")

		game.scoreboardMtx.RLock()
		defer game.scoreboardMtx.RUnlock()

		err := json.NewEncoder(w).Encode(game.OverallState)
		return err
	}))

	// GET /api/scoreboard/{tick} returns the scoreboard for a specific tick.
	handler.HandleFunc("GET /api/scoreboard/{tick}", game.route(func(w http.ResponseWriter, r *http.Request, user *User, team *Team) error {
		tickStr := r.PathValue("tick")

		tick, err := strconv.Atoi(tickStr)
		if err != nil {
			http.Error(w, "invalid tick", http.StatusBadRequest)
			return nil
		}

		w.Header().Set("Content-Type", "application/json")

		game.scoreboardMtx.RLock()
		defer game.scoreboardMtx.RUnlock()

		if tick > len(game.Ticks) {
			http.Error(w, "tick not found", http.StatusNotFound)
			return nil
		}

		err = json.NewEncoder(w).Encode(game.Ticks[tick-1])
		return err
	}))

	for path, pageInfo := range game.Config.Pages {
		if path == "/" {
			continue
		}

		handler.HandleFunc("GET "+path, game.route(func(w http.ResponseWriter, r *http.Request, user *User, team *Team) error {
			body, err := game.renderPage(r.Context(), path)
			if err != nil {
				return err
			}

			page := game.publicPageLayout(pageInfo.Title, user, body)

			err = htm.Render(r.Context(), w, page)
			return err
		}))
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
