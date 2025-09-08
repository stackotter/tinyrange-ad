package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tinyrange/ad/pkg/common"
	"golang.org/x/crypto/ssh"
)

type Steal struct {
	ID              int
	Flag            FlagInfo
	AttackingTeamID int
	StealTick       int
	StealTime       time.Time
}

type FlagInfo struct {
	TeamId    int `json:"teamId"`
	TickId    int `json:"tickId"`
	ServiceId int `json:"serviceId"`
}

type ServiceSummary struct {
	ID               int     `json:"id"`
	Points           float64 `json:"points"`
	TickPoints       float64 `json:"tickPoints"`
	AttackPoints     float64 `json:"attackPoints"`
	DefensePoints    float64 `json:"defensePoints"`
	UptimePercentage float64 `json:"uptimePercentage"`
}

type ServiceState struct {
	// Number of flags stolen from this service.
	FlagsLost int `json:"flagsLost"`

	// A list of flags for this service stolen from other teams.
	StolenFlags []FlagInfo `json:"stolenFlags"`

	SuccessfulUptimeChecks int `json:"successfulUptimeChecks"`
	FailedUptimeChecks     int `json:"failedUptimeChecks"`
}

func (s ServiceState) Summarize(completedTicks int64, id int, scoring ScoringConfig) ServiceSummary {
	tickPoints := float64(completedTicks) * scoring.PointsPerTick
	attackPoints := float64(len(s.StolenFlags)) * scoring.PointsPerStolenFlag
	defensePoints := float64(s.FlagsLost) * scoring.PointsPerLostFlag
	totalChecks := s.SuccessfulUptimeChecks + s.FailedUptimeChecks
	uptimePercentage := float64(s.SuccessfulUptimeChecks) / float64(totalChecks)
	if totalChecks == 0 {
		uptimePercentage = 1
	}
	points := (tickPoints + attackPoints + defensePoints) * uptimePercentage
	return ServiceSummary{
		ID:               id,
		Points:           points,
		TickPoints:       tickPoints,
		AttackPoints:     attackPoints,
		DefensePoints:    defensePoints,
		UptimePercentage: uptimePercentage,
	}
}

type TeamSummary struct {
	ID       int                     `json:"id"`
	Points   float64                 `json:"points"`
	Position int                     `json:"position"`
	Services map[int]*ServiceSummary `json:"services"`
}

type TeamState struct {
	Services map[int]*ServiceState
}

func (t TeamState) Summarize(completedTicks int64, id int, scoring ScoringConfig) TeamSummary {
	services := make(map[int]*ServiceSummary)
	total := float64(0)
	for serviceId, service := range t.Services {
		summary := service.Summarize(completedTicks, serviceId, scoring)
		services[serviceId] = &summary
		total += summary.Points
	}
	return TeamSummary{
		ID:       id,
		Points:   total,
		Position: 0,
		Services: services,
	}
}

func (t *TeamState) GetService(id int) *ServiceState { return t.Services[id] }

type ScoreboardSummary struct {
	Tick  int64         `json:"tick"`
	Teams []TeamSummary `json:"teams"`
}

type ScoreboardState struct {
	Tick  int64
	Teams map[int]*TeamState
}

func (s ScoreboardState) Summarize(completedTicks int64, teams map[int]*Team, scoring ScoringConfig) ScoreboardSummary {
	var teamSummaries []TeamSummary
	for id, team := range s.Teams {
		teamSummaries = append(teamSummaries, team.Summarize(completedTicks, id, scoring))
	}

	// Sort the teams by score and then by name if tied
	slices.SortFunc(teamSummaries, func(a, b TeamSummary) int {
		diff := int(b.Points) - int(a.Points)
		if diff == 0 {
			return strings.Compare(teams[a.ID].DisplayName, teams[b.ID].DisplayName)
		} else {
			return diff
		}
	})

	// Assign each team a position. Tied teams get the same position, but get sorted alphabetically so that the scoreboard ordering is stable.
	prevPoints := float64(0)
	position := 1
	actualPosition := 1
	for i, team := range teamSummaries {
		if i != 0 && team.Points != prevPoints {
			position = actualPosition
		}
		actualPosition += 1
		teamSummaries[i].Position = position
		prevPoints = team.Points
	}

	return ScoreboardSummary{
		Tick:  s.Tick,
		Teams: teamSummaries,
	}
}

func (s *ScoreboardState) GetTeam(id int) *TeamState { return s.Teams[id] }

type AttackDefenseGame struct {
	// Persist is the database for the game to persist state.
	Persist *PersistDatabase

	// PersistenceDir is the directory to store game related files in.
	PersistenceDir string

	// Config is the configuration for the game.
	Config Config

	// TinyRangePath is the path to the tinyrange binary.
	TinyRangePath string

	// TinyRangeVMMPath is the path to the tinyrange driver binary.
	TinyRangeVMMPath string

	// IP is the IP that the game listens on.
	ListenIP string

	// ExternalIP is the IP that contestants will use to join the game. May differ from ListenIP if using a proxy to expose the game to the internet etc.
	ExternalIP string

	// PublicPort is the port used for the frontend of the game.
	PublicPort int

	// Signer is the signer for the game.
	Signer *Signer

	// FlagGen is the flag generator for the game.
	FlagGen *FlagGenerator

	// CurrentTick is the current tick of the game.
	CurrentTick int64

	// EventQueue is the queue of events to run.
	EventQueue []TimelineEvent

	// Events is a map of events to run.
	Events map[string]*Event

	// Teams is a map of teams in the game. Make sure to keep in sync with the db.
	Teams map[int]*Team

	// AdminTeam is the admin team. Make sure to keep in sync with the db if JoinToken changes.
	AdminTeam Team

	// Router is the wireguard router for the game.
	Router WireguardRouter

	// Flow is the flow router for the game.
	Flow *FlowRouter

	// RouterMTU is the MTU(Maximum Transmission Unit) for the router.
	RouterMTU int

	templateMutex sync.Mutex
	// TinyRangeTemplates is a map of tinyrange templates that are already cached.
	// It points to the VM config filename.
	tinyRangeTemplates map[string]string

	// instanceMutex is used to guard access to instances, teamInstances, socInstances, botInstances, and devices
	instanceMutex sync.RWMutex

	// instances is a list of TinyRange instances.
	instances []TinyRangeInstance

	// Each of teamInstances, socInstances and botInstances holds indices into instances, keyed by team id.
	teamInstances map[int]int
	socInstances  map[int]int
	botInstances  map[int]int

	// devices is a list of external devices in the game.
	devices []*Device

	// SshServer is the SSH server used for admin connections.
	SshServer string

	// SshServerHostKey is the host key for the SSH server.
	SshServerHostKey string

	// TimeScale is the time scale to run the game at.
	TimeScale float64

	// NoInstances disables instance running and just runs the website + vpn.
	NoInstances bool

	// The state of the game (stopped, starting, or started)
	RunningState atomic.Int32

	// The mutex used to protect scoreboard-related properties of AttackDefenseGame
	scoreboardMtx sync.RWMutex
	// A snapshot of the scoreboard after each completed tick
	ScoreboardTicks []ScoreboardState
	// The working state of the scoreboard
	WorkingScoreboard ScoreboardState
	// The scoreboard currently being displayed (i.e. the scoreboard as of the last completed tick)
	DisplayedScoreboard ScoreboardState
	// A summary of the currently displayed scoreboard (stored to avoid computing it multiple times)
	DisplayedScoreboardSummary ScoreboardSummary

	// Error that caused termination of game
	Error error

	publicServerMux *http.ServeMux
	publicServer    *http.Server

	rebuildTemplates bool

	// internal services
	internalWeb    *hostService
	pingService    *hostService
	flagSubmission *hostService
}

const (
	RunningStateStopped  int32 = 0
	RunningStateStarting int32 = 1
	RunningStateStarted  int32 = 2
)

func (game *AttackDefenseGame) PlayerTeams() []*Team {
	var teams []*Team
	for _, team := range game.Teams {
		if team.IsAdmin() {
			continue
		}
		teams = append(teams, team)
	}
	return teams
}

// Flows implements FlowInstance.
func (game *AttackDefenseGame) Flows() []ParsedFlow {
	// The host doesn't use flows to make connections.
	return nil
}

// Hostname implements FlowInstance.
func (game *AttackDefenseGame) Hostname() string {
	return "host"
}

// InstanceAddress implements FlowInstance.
func (game *AttackDefenseGame) InstanceAddress() net.IP {
	return net.ParseIP(HOST_IP)
}

// Services implements FlowInstance.
func (game *AttackDefenseGame) Services() []FlowService {
	return []FlowService{
		game.internalWeb,
		game.pingService,
		game.flagSubmission,
	}
}

// Tags implements FlowInstance.
func (game *AttackDefenseGame) Tags() TagList {
	return TagList{"public/host"}
}

func (game *AttackDefenseGame) FrontendUrl() string {
	return fmt.Sprintf("http://%s:%d", game.ListenIP, game.PublicPort)
}

func (game *AttackDefenseGame) ResolvePath(path string) string {
	return filepath.Join(game.Config.basePath, path)
}

func (game *AttackDefenseGame) getInstances() []TinyRangeInstance {
	game.instanceMutex.RLock()
	defer game.instanceMutex.RUnlock()
	instances := make([]TinyRangeInstance, len(game.instances))
	copy(instances, game.instances)
	return instances
}

func (game *AttackDefenseGame) GetEvents() []string {
	events := make([]string, 0, len(game.Events))

	for name := range game.Events {
		events = append(events, name)
	}

	return events
}

func (game *AttackDefenseGame) scaleDuration(dur time.Duration) time.Duration {
	return time.Duration(float64(dur.Nanoseconds()) * game.TimeScale)
}

func (game *AttackDefenseGame) teamFromTag(tag string) (team *Team, bot bool, err error) {
	if strings.HasPrefix(tag, "team/") {
		tag = strings.TrimPrefix(tag, "team/")
	} else if strings.HasPrefix(tag, "bot/") {
		tag = strings.TrimPrefix(tag, "bot/")
		bot = true
	} else if strings.HasPrefix(tag, "device/") {
		tag = strings.TrimPrefix(tag, "device/")
	} else {
		return nil, false, fmt.Errorf("invalid tag: %s", tag)
	}

	for _, t := range game.Teams {
		if t.DisplayName == tag {
			return t, bot, nil
		}
	}

	return nil, false, fmt.Errorf("team not found: %s", tag)
}

func (game *AttackDefenseGame) flagsStolenBy(teamId int, serviceId int) []FlagInfo {
	return game.WorkingScoreboard.Teams[teamId].Services[serviceId].StolenFlags
}

func (game *AttackDefenseGame) submitFlag(submittingTeamId int, flag string) FlagStatus {
	if game.RunningState.Load() != RunningStateStarted {
		return GameNotRunning
	}

	// Locking the mutex now ensures the flag is counted for the current tick.
	// It also ensures that the flag is not counted twice.
	// And it means if a new tick if about to start the flag will be counted for the current tick.
	game.scoreboardMtx.Lock()
	defer game.scoreboardMtx.Unlock()

	tickId, teamId, serviceId, ok := game.FlagGen.Verify(game.Signer.Public(), flag)
	if !ok {
		return InvalidFlag
	}

	if teamId == submittingTeamId {
		return FlagFromOwnTeam
	}

	if serviceId < 0 || serviceId >= len(game.Config.Vulnbox.Services) {
		return InvalidService
	}

	if int64(tickId) < game.CurrentTick-game.FlagValidTicks() {
		return FlagExpired
	}

	if int64(tickId) > game.CurrentTick {
		return FlagNotYetValid
	}

	// Check if the flag has already been stolen.
	for _, stolen := range game.flagsStolenBy(submittingTeamId, serviceId) {
		if stolen.TeamId == teamId && stolen.TickId == tickId {
			return FlagAlreadyStolen
		}
	}

	ownService := game.WorkingScoreboard.Teams[submittingTeamId].Services[serviceId]
	ownService.StolenFlags = append(ownService.StolenFlags,
		FlagInfo{TeamId: teamId, TickId: tickId, ServiceId: serviceId},
	)

	loserService := game.WorkingScoreboard.Teams[teamId].Services[serviceId]
	loserService.FlagsLost += 1

	return FlagAccepted
}

func (game *AttackDefenseGame) cacheTinyRangeTemplate(templateFilename string, ram string) error {
	game.templateMutex.Lock()
	defer game.templateMutex.Unlock()

	resolvedFilename := game.ResolvePath(templateFilename)

	args := []string{
		game.TinyRangePath, "login",
		"--template",
		"--load-config", resolvedFilename,
		"--storage", "4096",
	}

	if ram != "" {
		args = append(args, "--ram", ram)
	}

	if *verbose {
		args = append(args, "--verbose")
	}

	if game.rebuildTemplates {
		args = append(args, "--rebuild")
	}

	// Run `tinyrange login --template --load-config <templateFilename>` to cache the template.
	cmd := exec.Command(args[0], args[1:]...)

	cmd.Stderr = os.Stderr

	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("failed to cache tinyrange template: %w", err)
	}

	// get only the last line in out.
	lines := strings.Split(string(out), "\n")
	if len(lines) == 0 {
		return fmt.Errorf("failed to cache tinyrange template: no output")
	}

	last := lines[len(lines)-1]
	if last == "" {
		last = lines[len(lines)-2]
	}

	game.tinyRangeTemplates[templateFilename] = last

	return nil
}

func (game *AttackDefenseGame) getCachedTemplate(templateFilename string) (string, bool) {
	game.templateMutex.Lock()
	defer game.templateMutex.Unlock()

	filename, ok := game.tinyRangeTemplates[templateFilename]
	if !ok {
		return "", false
	}

	return filename, true
}

func (game *AttackDefenseGame) ensureTemplateCached(templateFilename string, ram string) error {
	if _, ok := game.getCachedTemplate(templateFilename); !ok {
		if err := game.cacheTinyRangeTemplate(templateFilename, ram); err != nil {
			return err
		}
	}

	return nil
}

func (game *AttackDefenseGame) StartTeamInstanceFromConfig(name string, ip string, config InstanceConfig, team Team, initCommandTemplate string, services []ServiceConfig) (TinyRangeInstance, error) {
	inst, err := game.StartInstanceFromConfig(name, ip, config, team.ID)
	if err != nil {
		return nil, err
	}

	if err := inst.ParseFlowsAsTeam(team); err != nil {
		return nil, fmt.Errorf("failed to parse flows for team (%d): %w", team.ID, err)
	}

	for _, service := range services {
		inst.AddService(&service)
	}

	if err := team.runInitCommand(inst, initCommandTemplate); err != nil {
		return nil, fmt.Errorf("failed to run init command for team: %w", err)
	}

	return inst, nil
}

func (game *AttackDefenseGame) StartInstanceFromConfig(name string, ip string, config InstanceConfig, teamID int) (TinyRangeInstance, error) {
	// Check if the template is already cached.
	if err := game.ensureTemplateCached(config.Template, config.Ram); err != nil {
		return nil, err
	}

	// Start the instance.
	game.instanceMutex.Lock()
	id := len(game.instances)
	inst, err := NewTinyRangeInstance(game, name, net.ParseIP(ip), config, teamID, id)
	if err != nil {
		return nil, err
	}
	game.instances = append(game.instances, inst)
	game.instanceMutex.Unlock()

	slog.Info("starting instance", "template", config.Template, "instance", inst, "name", name)

	handler, err := game.Flow.AddInstance(inst)
	if err != nil {
		return nil, err
	}

	wg, err := game.Router.AddEndpoint(handler, VM_IP)
	if err != nil {
		return nil, err
	}

	if err != nil {
		return nil, err
	}

	if err := inst.Start(config.Template, wg); err != nil {
		return nil, err
	}

	return inst, nil
}

// RunEvent runs the event with the given name.
func (game *AttackDefenseGame) RunEvent(ctx context.Context, name string) error {
	ev, ok := game.Events[name]
	if !ok {
		return fmt.Errorf("event %s not implemented", name)
	}

	return ev.Run(ctx, game)
}

// TotalTicks returns the total number of ticks in the game.
func (game *AttackDefenseGame) TotalTicks() int64 {
	return game.Config.Duration.Nanoseconds() / game.Config.TickRate.Nanoseconds()
}

func (game *AttackDefenseGame) FlagValidTicks() int64 {
	return game.Config.FlagValidTime.Nanoseconds() / game.Config.TickRate.Nanoseconds()
}

func (game *AttackDefenseGame) AddTeam(name string) error {
	joinToken, err := GenerateJoinToken()
	if err != nil {
		return err
	}
	team := Team{
		DisplayName: name,
		JoinToken:   joinToken,
	}
	_, err = game.Persist.InsertTeam(&team)
	if err != nil {
		return err
	}
	game.Teams[team.ID] = &team
	return nil
}

func (game *AttackDefenseGame) AddEvent(name string, run EventCallback) {
	game.Events[name] = &Event{Run: run}
}

func (game *AttackDefenseGame) RemoveEvent(name string) {
	delete(game.Events, name)
}

// ForAllTeams runs the given function for each team in the game.
func (game *AttackDefenseGame) ForAllTeams(includeAdmin bool, includeBots bool, background bool, f func(t *Team, info TargetInfo) error) error {
	if background {
		for _, team := range game.Teams {
			if team.IsAdmin() && !includeAdmin {
				continue
			}

			go func(team *Team) {
				if err := f(team, team.Info()); err != nil {
					slog.Error("failed to run function for team", "team id", team.ID, "err", err)
				}
			}(team)

			if includeBots && game.Config.Vulnbox.Bot.Enabled {
				go func(team *Team) {
					if err := f(team, team.BotInfo()); err != nil {
						slog.Error("failed to run function for bot", "team id", team.ID, "err", err)
					}
				}(team)
			}
		}

		return nil
	} else {
		var wg sync.WaitGroup
		errChan := make(chan error, len(game.Teams))

		for _, team := range game.Teams {
			if team.IsAdmin() && !includeAdmin {
				continue
			}

			wg.Add(1)
			go func(team *Team) {
				defer wg.Done()
				if err := f(team, team.Info()); err != nil {
					slog.Error("failed to run function for team", "team id", team.ID, "err", err)
					errChan <- err
				}
			}(team)

			if includeBots && game.Config.Vulnbox.Bot.Enabled {
				wg.Add(1)

				go func(team *Team) {
					defer wg.Done()
					if err := f(team, team.BotInfo()); err != nil {
						slog.Error("failed to run function for bot", "team id", team.ID, "err", err)
						errChan <- err
					}
				}(team)
			}
		}

		wg.Wait()
		close(errChan)

		if len(errChan) > 0 {
			return fmt.Errorf("failed to run function for each team")
		}

		return nil
	}
}

func (game *AttackDefenseGame) initializeScoreboard() {
	game.WorkingScoreboard = game.makeBlankScoreboard()
	game.DisplayedScoreboard = game.makeBlankScoreboard()
	game.DisplayedScoreboardSummary = game.DisplayedScoreboard.Summarize(0, game.Teams, game.Config.Scoring)
}

func (game *AttackDefenseGame) makeBlankScoreboard() ScoreboardState {
	scoreboard := ScoreboardState{
		Tick:  0,
		Teams: make(map[int]*TeamState),
	}

	for _, team := range game.PlayerTeams() {
		teamState := &TeamState{Services: make(map[int]*ServiceState)}
		botState := &TeamState{Services: make(map[int]*ServiceState)}
		for _, service := range game.Config.Vulnbox.Services {
			if service.Private {
				continue
			}
			serviceState := ServiceState{
				FlagsLost:              0,
				StolenFlags:            []FlagInfo{},
				SuccessfulUptimeChecks: 0,
				FailedUptimeChecks:     0,
			}
			teamState.Services[service.Id] = &serviceState
			if game.Config.Vulnbox.Bot.Enabled {
				botState.Services[service.Id] = &serviceState
			}
		}
		scoreboard.Teams[team.ID] = teamState

		if game.Config.Vulnbox.Bot.Enabled {
			scoreboard.Teams[team.BotId()] = botState
		}
	}

	return scoreboard
}

func (game *AttackDefenseGame) updateScoreboard() {
	game.scoreboardMtx.Lock()
	defer game.scoreboardMtx.Unlock()

	game.ScoreboardTicks = append(game.ScoreboardTicks, game.WorkingScoreboard)
	game.DisplayedScoreboard = game.WorkingScoreboard
	game.DisplayedScoreboardSummary = game.WorkingScoreboard.Summarize(game.CurrentTick, game.Teams, game.Config.Scoring)
}

func (game *AttackDefenseGame) EndTick() {
	if game.CurrentTick >= game.TotalTicks() {
		return
	}

	game.updateScoreboard()
	game.CurrentTick += 1
}

func (game *AttackDefenseGame) StartTick() error {
	slog.Info("tick", "num", game.CurrentTick)

	ctx, cancel := context.WithTimeout(context.Background(), game.scaleDuration(game.Config.TickRate.Duration))
	defer cancel()

	start := time.Now()

	// Run any events that are scheduled for this tick at the start of the tick.
	for _, ev := range game.EventQueue {
		if ev.Tick(game) == game.CurrentTick {
			if err := ev.Run(ctx, game); err != nil {
				slog.Error("failed to run event", "name", ev.Event, "err", err)
			}
		}
	}

	// Send the scorebot command to each team.
	if err := game.ForAllTeams(false, true, false, func(t *Team, info TargetInfo) error {
		// Run the scorebot for each service.
		if err := game.Config.ScoreBot.ForEachService(func(service *ScoreBotServiceConfig) error {
			// Delay this randomly during the tick interval.
			totalTickTime := game.scaleDuration(game.Config.TickRate.Duration)

			delay := time.Duration(rand.Intn(int(totalTickTime.Milliseconds())/2)) * time.Millisecond

			time.Sleep(delay)

			start := time.Now()

			subCtx, cancel := context.WithTimeout(ctx, game.scaleDuration(service.Timeout.Duration))
			defer cancel()

			tickId := int(game.CurrentTick)
			newFlag := game.FlagGen.Generate(tickId, info.ID, service.Id, game.Signer)

			success, message, err := service.Run(subCtx, &game.Config.ScoreBot, game, info, newFlag)
			if err != nil {
				slog.Error("failed to run scorebot", "err", err)
				success = false
			}

			slog.Info("scorebot response",
				"team", info.Name,
				"service", service.Id,
				"success", success,
				"message", message,
				"duration", time.Since(start),
			)

			// Update the service state.
			game.scoreboardMtx.Lock()
			serviceState := game.WorkingScoreboard.Teams[info.ID].Services[service.Id]
			if success {
				serviceState.SuccessfulUptimeChecks += 1
			} else {
				serviceState.FailedUptimeChecks += 1
			}
			game.scoreboardMtx.Unlock()

			return nil
		}); err != nil {
			return err
		}

		return nil
	}); err != nil {
		slog.Error("failed to run scorebot for each team", "err", err)
	}

	dur := time.Since(start)

	if dur > game.scaleDuration(game.Config.TickRate.Duration) {
		slog.Warn("tick took too long", "duration", dur)
	}

	return nil
}

func (game *AttackDefenseGame) GenerateKeys() error {
	signer, err := GenerateKey()
	if err != nil {
		return err
	}

	game.Signer = signer

	game.FlagGen = NewFlagGenerator("flag{", "}")

	return nil
}

func (game *AttackDefenseGame) teamInstance(teamID int) *TinyRangeInstance {
	game.instanceMutex.RLock()
	defer game.instanceMutex.RUnlock()
	return game.lookupInstance(teamID, game.teamInstances, false)
}

func (game *AttackDefenseGame) botInstance(teamID int) *TinyRangeInstance {
	game.instanceMutex.RLock()
	defer game.instanceMutex.RUnlock()
	return game.lookupInstance(teamID, game.botInstances, false)
}

func (game *AttackDefenseGame) socInstance(teamID int) *TinyRangeInstance {
	game.instanceMutex.RLock()
	defer game.instanceMutex.RUnlock()
	return game.lookupInstance(teamID, game.socInstances, false)
}

func (game *AttackDefenseGame) lookupInstance(teamID int, idMap map[int]int, acquireLock bool) *TinyRangeInstance {
	id, ok := idMap[teamID]
	if !ok {
		return nil
	}
	if acquireLock {
		game.instanceMutex.RLock()
	}
	inst := &game.instances[id]
	if acquireLock {
		game.instanceMutex.RUnlock()
	}
	return inst
}

func (game *AttackDefenseGame) GetSSHConfig(teamID int) (SecureSSHConfig, error) {
	inst := game.teamInstance(teamID)
	if inst == nil {
		return SecureSSHConfig{}, fmt.Errorf("team instance not set")
	}

	return (*inst).SecureConfig(), nil
}

func (game *AttackDefenseGame) instanceFromName(name string) (TinyRangeInstance, error) {
	if name == "scorebot" {
		return game.Config.ScoreBot.instance, nil
	}

	game.instanceMutex.RLock()
	defer game.instanceMutex.RUnlock()
	for _, inst := range game.instances {
		if name == inst.Hostname() {
			return inst, nil
		}
	}

	return nil, fmt.Errorf("instance not found")
}

func (game *AttackDefenseGame) startSshServer() error {
	slog.Info("starting ssh server", "addr", game.SshServer)

	listen, err := net.Listen("tcp", game.SshServer)
	if err != nil {
		return fmt.Errorf("failed to listen: %w", err)
	}

	config := &ssh.ServerConfig{
		NoClientAuth: true,
		NoClientAuthCallback: func(conn ssh.ConnMetadata) (*ssh.Permissions, error) {
			return nil, nil
		},
	}

	privateBytes, err := os.ReadFile(game.SshServerHostKey)
	if err != nil {
		return fmt.Errorf("failed to read private key: %w", err)
	}

	private, err := ssh.ParsePrivateKey(privateBytes)
	if err != nil {
		return fmt.Errorf("failed to parse private key: %w", err)
	}

	config.AddHostKey(private)

	go func() {
		for {
			conn, err := listen.Accept()
			if err != nil {
				slog.Error("failed to accept connection", "err", err)
				return
			}

			slog.Info("accepted connection", "remote", conn.RemoteAddr())

			go func() {
				sshConn, chans, reqs, err := ssh.NewServerConn(conn, config)
				if err != nil {
					slog.Error("failed to handshake", "err", err)
					return
				}

				_ = sshConn

				go ssh.DiscardRequests(reqs)

				for newChannel := range chans {
					if newChannel.ChannelType() == "direct-tcpip" {
						data := newChannel.ExtraData()

						hostnameLen := binary.BigEndian.Uint32(data[:4])
						hostname := string(data[4 : 4+hostnameLen])

						instance, err := game.instanceFromName(hostname)
						if err != nil {
							_ = newChannel.Reject(ssh.UnknownChannelType, fmt.Sprintf("instance not found: %s", hostname))
							return
						}

						other, err := game.DialContext(context.Background(), nil, "tcp", ipPort(instance.Hostname(), VM_SSH_PORT))
						if err != nil {
							_ = newChannel.Reject(ssh.UnknownChannelType, fmt.Sprintf("failed to dial instance: %s", err))
							return
						}

						chn, reqs, err := newChannel.Accept()
						if err != nil {
							slog.Error("failed to accept channel", "err", err)
							return
						}
						defer chn.Close()

						go ssh.DiscardRequests(reqs)

						if err := common.Proxy(chn, other, 4096); err != nil {
							slog.Error("failed to proxy", "err", err)
						}
					} else {
						_ = newChannel.Reject(ssh.UnknownChannelType, fmt.Sprintf("unknown channel type: %s", newChannel.ChannelType()))
						return
					}
				}
			}()
		}
	}()

	return nil
}

func (game *AttackDefenseGame) Run() error {
	// Ensure we clean up all instances when we're done.
	defer func() {
		game.instanceMutex.RLock()
		defer game.instanceMutex.RUnlock()
		for _, inst := range game.instances {
			if err := inst.Stop(); err != nil {
				slog.Error("failed to stop instance", "hostname", inst.Hostname(), "err", err)
			}
		}
	}()

	var err error

	// Generate a key using age for the game.
	// This key will be used to sign the flags.
	if err := game.GenerateKeys(); err != nil {
		return fmt.Errorf("failed to generate keys: %w", err)
	}

	if game.RouterMTU == 0 {
		game.RouterMTU = 1420
	}

	game.Router, err = NewWireguardRouter(game.ListenIP, game.ExternalIP, game.RouterMTU, game.FrontendUrl())
	if err != nil {
		return fmt.Errorf("failed to create wireguard router: %w", err)
	}

	game.Flow = NewFlowRouter()

	if _, err := game.Flow.AddInstance(game); err != nil {
		return fmt.Errorf("failed to add host to flow router: %w", err)
	}

	// Start the built in web server.
	if err := game.startPublicServer(); err != nil {
		return fmt.Errorf("failed to start server: %w", err)
	}

	// Register the services on wireguard player network.
	if err := game.registerInternalServices(); err != nil {
		return fmt.Errorf("failed to register internal services: %w", err)
	}

	if game.SshServer != "" {
		// Start the SSH server.
		if err := game.startSshServer(); err != nil {
			return fmt.Errorf("failed to start ssh server: %w", err)
		}
	}

	// Register events for the game.
	if game.Config.Vulnbox.Bot.Enabled {
		for name, ev := range game.Config.Vulnbox.Bot.Events {
			game.AddEvent(fmt.Sprintf("bot/%s", name), func(ctx context.Context, game *AttackDefenseGame) error {
				return game.ForAllTeams(false, false, ev.Background, func(t *Team, info TargetInfo) error {
					subCtx, cancel := context.WithTimeout(ctx, game.scaleDuration(ev.Timeout.Duration))
					defer cancel()

					return t.runBotCommand(subCtx, game, t.Info(), t.BotInfo(), ev.Command)
				})
			})
		}
	}

	// Sort the timeline events by tick.
	game.EventQueue = game.Config.Timeline
	slices.SortFunc(game.EventQueue, func(a TimelineEvent, b TimelineEvent) int {
		return int(a.Tick(game) - b.Tick(game))
	})

	// Load all existing device configurations.
	if err := game.Persist.ForEachDevice(func(device DeviceConfig) error {
		dev, err := game.createDevice(device)
		dev.id = device.ID
		if err != nil {
			return fmt.Errorf("failed to add device: %w", err)
		}

		handler, err := game.Flow.AddInstance(dev)
		if err != nil {
			return fmt.Errorf("failed to add device to flow router: %w", err)
		}

		if device.Config == nil {
			return fmt.Errorf("device missing wireguard config string: id=%d, name=%q", device.ID, device.Name)
		}

		wg, err := game.Router.RestoreDevice(*device.Config, handler)
		if err != nil {
			return err
		}

		dev.wg = wg
		game.instanceMutex.Lock()
		game.devices = append(game.devices, dev)
		game.instanceMutex.Unlock()

		return nil
	}); err != nil {
		return fmt.Errorf("failed to load devices: %w", err)
	}

	return nil
}

func (game *AttackDefenseGame) Start() error {
	if !game.RunningState.CompareAndSwap(RunningStateStopped, RunningStateStarting) {
		return fmt.Errorf("already started")
	}

	game.Error = nil
	game.CurrentTick = 1
	game.instances = []TinyRangeInstance{}
	game.initializeScoreboard()

	defer game.RunningState.Store(RunningStateStopped)

	// Boot the scorebot.
	if err := game.Config.ScoreBot.Start(game); err != nil {
		return fmt.Errorf("failed to start scorebot: %w", err)
	}

	// Wait for the scorebot to boot.
	if err := game.Config.ScoreBot.Wait(); err != nil {
		return fmt.Errorf("failed to wait for scorebot: %w", err)
	}

	// Initialize all initial teams.
	if err := game.ForAllTeams(false, false, false, func(t *Team, info TargetInfo) error {
		return t.Start(game)
	}); err != nil {
		return fmt.Errorf("failed to start team instances: %w", err)
	}

	if game.Config.Wait {
		// Wait for a event to start the game.
		slog.Info("waiting for event to start game")

		start := make(chan struct{})

		game.AddEvent("start", func(ctx context.Context, game *AttackDefenseGame) error {
			close(start)
			game.RemoveEvent("start")
			return nil
		})

		<-start
	}

	// Log the start of the game.
	slog.Info("game starting", "completesAt", time.Now().Add(game.scaleDuration(game.Config.Duration.Duration)), "totalTicks", game.TotalTicks())

	game.RunningState.Store(RunningStateStarted)

	// Create a new ticker for the game.
	ticker := time.NewTicker(game.scaleDuration(game.Config.TickRate.Duration))

	// Create a new timer for the end of the game.
	endTime := time.NewTimer(game.scaleDuration(game.Config.Duration.Duration))

	// First tick
	if err := game.StartTick(); err != nil {
		slog.Error("failed to tick", "err", err)
	}

outer:
	for {
		select {
		case <-ticker.C:
			// End previous tick
			game.EndTick()

			// Start next tick
			if err := game.StartTick(); err != nil {
				slog.Error("failed to tick", "err", err)
			}
		case <-endTime.C:
			game.EndTick()
			break outer
		}
	}

	slog.Info("game complete")
	return nil
}

func (game *AttackDefenseGame) createDevice(device DeviceConfig) (*Device, error) {
	dev := &Device{
		game:   game,
		id:     device.ID,
		name:   device.Name,
		userID: device.UserID,
	}

	if err := dev.ParseTags(); err != nil {
		return nil, fmt.Errorf("failed to parse tags for device (%s): %w", device.Name, err)
	}

	if err := dev.ParseFlows(); err != nil {
		return nil, fmt.Errorf("failed to parse flows for device (%s): %w", device.Name, err)
	}

	return dev, nil
}

func (game *AttackDefenseGame) AddDevice(name string, userID int) error {
	device := &DeviceConfig{
		UserID: userID,
		Name:   name,
	}
	id, err := game.Persist.InsertDevice(device)
	if err != nil {
		return err
	}

	dev, err := game.createDevice(*device)
	if err != nil {
		return err
	}

	handler, err := game.Flow.AddInstance(dev)
	if err != nil {
		return err
	}

	wg, deviceConfig, err := game.Router.AddDevice(handler)
	if err != nil {
		return err
	}

	if err := game.Persist.UpdateDeviceConfig(id, &deviceConfig); err != nil {
		return err
	}

	dev.wg = wg
	game.instanceMutex.Lock()
	game.devices = append(game.devices, dev)
	game.instanceMutex.Unlock()

	slog.Info("added device", "name", name, "hostname", dev.Hostname())

	return nil
}

func (game *AttackDefenseGame) DialContext(ctx context.Context, source FlowInstance, network, address string) (net.Conn, error) {
	return game.Flow.DialContext(ctx, source, network, address)
}

func (game *AttackDefenseGame) GetDevices() []WireguardDevice {
	game.instanceMutex.RLock()
	defer game.instanceMutex.RUnlock()

	devices := make([]WireguardDevice, 0, len(game.devices))

	for _, dev := range game.devices {
		devices = append(devices, WireguardDevice{
			ID:        dev.id,
			UserID:    dev.userID,
			ConfigUrl: dev.wg.ConfigUrl(),
			Name:      dev.name,
			IP:        dev.InstanceAddress().String(),
		})
	}

	return devices
}

var (
	_ FlowInstance = &AttackDefenseGame{}
)
