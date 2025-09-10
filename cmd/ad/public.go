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
	"golang.org/x/crypto/ssh"
	"gopkg.in/yaml.v3"
)

type flagIdApiResponse struct {
	Tick    int    `json:"tick"`
	Team    int    `json:"team"`
	Service int    `json:"service"`
	Value   string `json:"value"`
}

type serviceApiResponse struct {
	Id   int    `json:"id"`
	Name string `json:"name"`
	Port int    `json:"port"`
}

type teamApiResponse struct {
	Self bool   `json:"self"`
	Id   int    `json:"id"`
	IP   string `json:"ip"`
	Name string `json:"name"`
}

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

func (game *AttackDefenseGame) requireTeam(w http.ResponseWriter, r *http.Request) (Team, error) {
	teamInfo, ok := r.Context().Value(CONTEXT_KEY_TEAM).(TargetInfo)
	if !ok {
		_, team, err := game.requireAuthentication(w, r)
		if err != nil {
			return Team{}, err
		} else {
			return team, nil
		}
	} else {
		return game.Persist.GetTeam(teamInfo.ID)
	}
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
	if game.RunningState.Load() != RunningStateStarted {
		return nil
	}

	game.scoreboardMtx.RLock()
	scoreboard := game.DisplayedScoreboardSummary
	game.scoreboardMtx.RUnlock()

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
	for _, team := range scoreboard.Teams {
		row := htm.Group{
			html.Textf("%d", team.Position),
			html.Textf("%s", game.Teams[team.ID].DisplayName),
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
					html.Textf("%d%%", int(serviceState.UptimePercentage*100)),
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
			// bootstrap.NavbarLink("/events", html.Text("Events")),
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
		)

		if !game.isAdmin(*user) {
			navitems = append(navitems, bootstrap.NavbarLink("/team", html.Text("Team")))
		}

		navitems = append(navitems,
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

func (game *AttackDefenseGame) teamRoute(
	handler func(w http.ResponseWriter, r *http.Request, team Team) error,
) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		team, err := game.requireTeam(w, r)
		if err != nil {
			game.renderError(w, r, err, nil)
			return
		}
		err = handler(w, r, team)
		if err != nil {
			game.renderError(w, r, err, nil)
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
		instanceList = append(instanceList, bootstrap.CardTitle("Instances"))

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

		page := game.publicPageLayout("Instances", &user,
			bootstrap.Card(instanceList...),
		)

		err := htm.Render(r.Context(), w, page)
		return err
	}))

	// GET /game displays game management interface
	handler.HandleFunc("GET /game", game.adminRoute(func(w http.ResponseWriter, r *http.Request, user User, team Team) error {
		var content []htm.Fragment
		content = append(content,
			html.Div(htm.Class("mb-4"),
				bootstrap.CardTitle("Manage game"),
			),
		)

		runningState := game.RunningState.Load()
		if runningState == RunningStateStarted {
			content = append(content,
				html.P(htm.Text("Game is running")),
			)
		} else if runningState == RunningStateStarting {
			content = append(content,
				html.P(htm.Text("Game is starting")),
			)
		} else if runningState == RunningStateResetting {
			content = append(content,
				html.P(htm.Text("Game is resetting")),
			)
		}

		content = append(content,
			html.P(html.Textf("Tick %d out of %d", game.CurrentTick, game.TotalTicks())),
		)

		if runningState == RunningStateStopped {
			content = append(content,
				html.Div(
					htm.Attr("style", "display: flex; gap: 1rem"),
					html.Form(
						html.FormTarget("POST", "/api/game/start"),
						bootstrap.SubmitButton("Start", bootstrap.ButtonColorPrimary),
					),
					html.Form(
						html.FormTarget("POST", "/api/game/reset"),
						bootstrap.SubmitButton("Reset", bootstrap.ButtonColorDanger),
					),
				),
			)
			if game.Error != nil {
				content = append(content,
					html.P(html.Textf("Game failed to start: %v", game.Error)),
				)
			}
		}

		content = []htm.Fragment{bootstrap.Card(content...)}
		page := game.publicPageLayout("Game", &user, content...)

		err := htm.Render(r.Context(), w, page)
		return err
	}))

	// POST /api/game/start starts the game
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

	// POST /api/game/reset resets the game
	handler.HandleFunc("POST /api/game/reset", game.adminRoute(func(w http.ResponseWriter, r *http.Request, user User, team Team) error {
		if err := game.Reset(); err != nil {
			slog.Error("Failed to reset game", "err", err)
			game.Error = err
		}

		http.Redirect(w, r, "/game", http.StatusFound)
		return nil
	}))

	// GET /teams lists all teams.
	handler.HandleFunc("GET /teams", game.authenticatedRoute(func(w http.ResponseWriter, r *http.Request, user User, team Team) error {
		isAdmin := game.isAdmin(user)

		var teams []*Team
		teams = slices.AppendSeq(teams, maps.Values(game.Teams))
		slices.SortFunc(teams, func(a *Team, b *Team) int {
			return a.ID - b.ID
		})

		var headerRow htm.Group
		headerRow = append(headerRow, htm.Text("Name"))
		if game.isAdmin(user) {
			headerRow = append(headerRow, htm.Text("Join token"))
		}
		headerRow = append(headerRow, htm.Text("IP"))

		for _, service := range game.Config.Vulnbox.Services {
			headerRow = append(headerRow, htm.Text(service.Name()))
		}

		for _, service := range game.Config.Socbox.Services {
			headerRow = append(headerRow, html.Text(service.Name()))
		}

		var teamList []htm.Group
		for _, team := range teams {
			if team.IsAdmin() && !isAdmin {
				continue
			}

			row := htm.Group{htm.Text(team.DisplayName)}

			if isAdmin {
				row = append(row, htm.Text(team.JoinToken))
			}

			if !team.IsAdmin() {
				row = append(row, htm.Text(team.IP()))

				for _, service := range game.Config.Vulnbox.Services {
					serviceUrl := fmt.Sprintf("http://%s:%d", team.IP(), service.Port())
					row = append(row, html.Link(serviceUrl, html.Textf("%s", serviceUrl)))
				}

				for _, service := range game.Config.Socbox.Services {
					serviceUrl := fmt.Sprintf("http://%s:%d", team.SocIP(), service.Port())
					row = append(row, html.Link(serviceUrl, html.Textf("%s", serviceUrl)))
				}
			} else {
				for _ = range len(game.Config.Vulnbox.Services) + len(game.Config.Socbox.Services) + 1 {
					row = append(row, html.Text("~"))
				}
			}

			teamList = append(teamList, row)
		}

		var content []htm.Fragment
		content = append(content,
			bootstrap.Card(
				bootstrap.CardTitle("Teams"),
				bootstrap.Table(
					headerRow,
					teamList,
				),
			),
		)

		if isAdmin && game.RunningState.Load() == RunningStateStopped {
			content = append(content,
				bootstrap.Card(
					bootstrap.CardTitle("Create team"),
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
			bootstrap.Card(
				bootstrap.CardTitle("Add device"),
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
			html.H2(html.Text("Register")),
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
			html.H2(html.Text("Log in")),
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
			bootstrap.Card(
				bootstrap.CardTitle("Profile"),
				html.Div(
					html.Div(html.Textf("Username: %s", user.Username)),
					html.Div(html.Textf("Team: %s", team.DisplayName)),
				),
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
		scoreboard := game.renderScoreboard()
		if scoreboard == nil {
			scoreboard = htm.Text("game has not started")
		}

		content := []htm.Fragment{
			bootstrap.Card(
				bootstrap.CardTitle("Scoreboard"),
				scoreboard,
			),
		}

		if user != nil && !game.isAdmin(*user) {
			content = append(content,
				bootstrap.Card(
					bootstrap.CardTitle("Submit Flag"),
					html.Form(
						html.Id("flag-form"),
						htmx.Post("/api/flag"),
						htmx.Target("flag-result"),
						bootstrap.FormField("Flag", "flag", html.FormOptions{
							Kind:     html.FormFieldText,
							Required: true,
							Value:    "",
						}),
						bootstrap.SubmitButton("Submit", bootstrap.ButtonColorPrimary),
					),
					html.Div(html.Id("flag-result")),
				),
			)
		}

		page := game.publicPageLayout("Scoreboard", user, content...)
		err := htm.Render(r.Context(), w, page)
		return err
	}))

	// GET /api/scoreboard returns the scoreboard for the overall state.
	handler.HandleFunc("GET /api/scoreboard", game.route(func(w http.ResponseWriter, r *http.Request, user *User, team *Team) error {
		w.Header().Set("Content-Type", "application/json")

		game.scoreboardMtx.RLock()
		defer game.scoreboardMtx.RUnlock()

		err := json.NewEncoder(w).Encode(game.DisplayedScoreboardSummary)
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

		if tick > len(game.ScoreboardTicks) {
			http.Error(w, "tick not found", http.StatusNotFound)
			return nil
		}

		summary := game.ScoreboardTicks[tick-1].Summarize(int64(tick-1), game.Teams, game.Config.Scoring)
		err = json.NewEncoder(w).Encode(summary)
		return err
	}))

	handler.HandleFunc("GET /api/teams", game.teamRoute(func(w http.ResponseWriter, r *http.Request, playerTeam Team) error {
		teams := make([]teamApiResponse, len(game.PlayerTeams()))

		for i, team := range game.PlayerTeams() {
			teams[i] = teamApiResponse{
				Self: team.ID == playerTeam.ID,
				Id:   team.ID,
				IP:   team.IP(),
				Name: team.DisplayName,
			}
		}

		json.NewEncoder(w).Encode(teams)
		return nil
	}))

	handler.HandleFunc("GET /api/vulnbox/services", func(w http.ResponseWriter, r *http.Request) {
		services := make([]serviceApiResponse, len(game.Config.Vulnbox.PublicServices()))
		for i, service := range game.Config.Vulnbox.PublicServices() {
			services[i] = serviceApiResponse{
				Id:   service.Id,
				Name: service.Name(),
				Port: service.Port(),
			}
		}

		json.NewEncoder(w).Encode(services)
	})

	// An endpoint for submitting flags.
	handler.HandleFunc("POST /api/flag", game.teamRoute(func(w http.ResponseWriter, r *http.Request, team Team) error {
		flag := r.FormValue("flag")
		if flag == "" {
			http.Error(w, "flag not found", http.StatusBadRequest)
			return nil
		}

		status, err := game.submitFlag(team.ID, flag)
		if err != nil {
			fmt.Fprintf(w, "Error (%v)\n", err)
		} else {
			fmt.Fprintf(w, "%s\n", status)
		}
		return nil
	}))

	// An API endpoint for listing current flag ids.
	handler.HandleFunc("GET /api/flagIds", func(w http.ResponseWriter, r *http.Request) {
		flagIds := make([]flagIdApiResponse, 0)
		for _, team := range game.PlayerTeams() {
			// Iterate through services with scorebot checks (those are the ones with flags)
			for _, serviceCheck := range game.Config.ScoreBot.Checks {
				service := game.Config.Vulnbox.GetService(serviceCheck.Id)
				if service == nil {
					http.Error(w, fmt.Sprintf("check %d doesn't have corresponding service", serviceCheck.Id), http.StatusInternalServerError)
					return
				}

				// Iterate through past few ticks. Exclude current tick because it may or may
				// not have been inserted yet and we want to avoid leaking flag ids before
				// they're used (otherwise people can create accounts with the same name on other
				// teams' vulnboxes before the scorebot gets around to it).
				for tickOffset := range game.FlagValidTicks() - 1 {
					if tickOffset >= game.CurrentTick-1 {
						continue
					}
					tickId := int(game.CurrentTick - tickOffset - 1)
					flag := game.FlagGen.Generate(tickId, team.ID, service.Id, game.Signer)
					flagId := flagIdApiResponse{
						Tick:    tickId,
						Team:    team.ID,
						Service: service.Id,
						Value:   GetFlagId(flag),
					}
					flagIds = append(flagIds, flagId)
				}
			}
		}

		json.NewEncoder(w).Encode(flagIds)
		return
	})

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

	game.publicServerMux = handler
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
