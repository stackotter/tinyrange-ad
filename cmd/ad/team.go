package main

import (
	"fmt"
	"log/slog"
	"net"
	"strings"
	"text/template"
	"time"

	"golang.org/x/net/context"
)

type TargetInfo struct {
	ID    int
	Name  string
	IP    string
	IsBot bool
	IsSoc bool
}

type Team struct {
	ID          int
	DisplayName string
	JoinToken   string
}

type Instance struct {
	ID        int
	TeamID    int
	Name      string
	sshConfig string
}

func GenerateJoinToken() (string, error) {
	return GenerateRandomString(32)
}

func (t *Team) IsAdmin() bool {
	return t.DisplayName == "admin"
}

func (t *Team) BotId() int { return t.ID + BOT_ID_OFFSET }

func (t *Team) SocId() int { return t.ID + SOC_ID_OFFSET }

func (t *Team) IP() string {
	return net.IPv4(10, 40, 10, 10+byte(t.ID)).String()
}

func (t *Team) SocIP() string {
	return net.IPv4(10, 40, 20, 10+byte(t.ID)).String()
}

func (t *Team) BotIP() string {
	return net.IPv4(10, 40, 30, 10+byte(t.ID)).String()
}

func (t *Team) Info() TargetInfo {
	return TargetInfo{
		ID:    t.ID,
		Name:  t.DisplayName,
		IP:    t.IP(),
		IsBot: false,
		IsSoc: false,
	}
}

func (t *Team) BotInfo() TargetInfo {
	return TargetInfo{
		ID:    t.BotId(),
		Name:  t.DisplayName + "_bot",
		IP:    t.BotIP(),
		IsBot: true,
		IsSoc: false,
	}
}

func (t *Team) SocInfo() TargetInfo {
	return TargetInfo{
		ID:    t.SocId(),
		Name:  t.DisplayName + "_soc",
		IP:    t.SocIP(),
		IsBot: false,
		IsSoc: true,
	}
}

func (t *Team) runBotCommand(ctx context.Context, game *AttackDefenseGame, teamInfo TargetInfo, botInfo TargetInfo, command string) error {
	commandTpl, err := template.New("command").Parse(command)
	if err != nil {
		return err
	}

	var buf strings.Builder

	if err := commandTpl.Execute(&buf, &struct {
		TargetIP    string
		IP          string
		TickSeconds float64
	}{
		TargetIP:    teamInfo.IP,
		IP:          botInfo.IP,
		TickSeconds: game.scaleDuration(game.Config.TickRate.Duration).Seconds(),
	}); err != nil {
		return err
	}

	botInstance := game.botInstance(t.ID)
	if botInstance == nil {
		return fmt.Errorf("team missing bot instance, team.DisplayName=%s", t.DisplayName)
	}

	// Run the command.
	resp, err := (*botInstance).RunCommand(ctx, buf.String())
	if err != nil {
		return fmt.Errorf("failed to run bot command(%w): %s", err, resp)
	}

	slog.Info("bot command response", "team", botInfo.Name, "response", resp)

	return nil
}

func (t *Team) runInitCommand(inst TinyRangeInstance, commandTemplate string) error {
	slog.Debug("running init command for team", "displayName", t.DisplayName)
	initTpl, err := template.New("init").Parse(commandTemplate)
	if err != nil {
		return err
	}

	var command strings.Builder
	if err := initTpl.Execute(&command, &struct {
		IP        string
		VulnboxIP string
		BotIP     string
		SocboxIP  string
		TeamName  string
	}{
		IP:        inst.InstanceAddress().String(),
		VulnboxIP: t.IP(),
		BotIP:     t.BotIP(),
		SocboxIP:  t.SocIP(),
		TeamName:  t.DisplayName,
	}); err != nil {
		return err
	}

	// Run the init command.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	resp, err := inst.RunCommand(ctx, command.String())
	if err != nil {
		return fmt.Errorf("failed to run init command: cmd=%q err=%w resp=%q", command.String(), err, resp)
	}

	if strings.Trim(resp, " \n") != "success" {
		return fmt.Errorf("init command failed: cmd=%q resp=%q", command.String(), resp)
	}

	return nil
}

func (t *Team) Start(game *AttackDefenseGame) error {
	// Start the team instance.
	inst, err := game.StartTeamInstanceFromConfig("team_"+t.DisplayName, t.IP(), game.Config.Vulnbox.InstanceConfig, *t, game.Config.Vulnbox.InitTemplate, game.Config.Vulnbox.Services)
	if err != nil {
		return err
	}
	game.instanceMutex.Lock()
	game.teamInstances[t.ID] = len(game.instances) - 1
	game.instanceMutex.Unlock()

	// Run a health check.
	if game.Config.Vulnbox.HealthCheck.Kind != HealthCheckKindNone {
		if err := inst.HealthCheck(game.Config.Vulnbox.HealthCheck); err != nil {
			return fmt.Errorf("failed to run health check for team: %w", err)
		}
	}

	// If there is a bot, start the bot instance.
	if game.Config.Vulnbox.Bot.Enabled {
		_, err := game.StartTeamInstanceFromConfig("team_"+t.DisplayName+"_bot", t.BotIP(), game.Config.Vulnbox.Bot.InstanceConfig, *t, game.Config.Vulnbox.InitTemplate, game.Config.Vulnbox.Services)
		if err != nil {
			return err
		}
		game.instanceMutex.Lock()
		game.botInstances[t.ID] = len(game.instances) - 1
		game.instanceMutex.Unlock()
	}

	// Start the soc instance.
	_, err = game.StartTeamInstanceFromConfig("team_"+t.DisplayName+"_soc", t.SocIP(), game.Config.Socbox.InstanceConfig, *t, game.Config.Socbox.InitTemplate, game.Config.Socbox.Services)
	if err != nil {
		return err
	}
	game.instanceMutex.Lock()
	game.socInstances[t.ID] = len(game.instances) - 1
	game.instanceMutex.Unlock()

	return nil
}
