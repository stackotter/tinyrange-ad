package main

import (
	"crypto/ed25519"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type PersistDatabase struct {
	conn *sql.DB
}

func CreateDatabaseConnection(file string) (*PersistDatabase, error) {
	dbExisted := false
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		dbExisted = true
	}

	conn, err := sql.Open("sqlite3", file)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %v", err)
	}

	db := &PersistDatabase{conn}
	if dbExisted {
		return db, nil
	}

	adminJoinToken, err := GenerateJoinToken()
	if err != nil {
		return nil, fmt.Errorf("failed to generate admin team join token: %v", err)
	}

	flagKey, err := GenerateKey()
	if err != nil {
		return nil, fmt.Errorf("failed to generate flag signing key: %v", err)
	}

	sqlStmt := `
	create table teams (
		id integer not null primary key,
		name text not null,
		join_token text not null
	);
	create table users (
		id integer not null primary key,
		team integer not null,
		username text not null,
		password_hash text not null
	);
	create table devices (
		id integer not null primary key,
		user integer not null,
		name text not null,
		config text
	);
	create table instances (
		id integer not null primary key,
		team integer not null,
		name text not null,
		ssh_config text not null
	);
	create table sessions (
		id integer not null primary key,
		user integer not null,
		token text not null,
		expires_at integer not null
	);
	create table state (
		tick integer not null,
		flag_key blob not null
	);
	create table flag_steals (
		id integer not null primary key,
		attacking_team integer not null,
		defending_team integer not null,
		service integer not null,
		steal_tick integer not null,
		flag_tick integer not null,
		time integer not null
	);
	create table uptime_checks (
		id integer not null primary key,
		team integer not null,
		service integer not null,
		tick integer not null,
		start_time integer not null,
		duration real not null,
		success integer not null,
		failure_reason text
	);

	insert into state(tick, flag_key) values(1, ?);
	insert into teams(name, join_token) values('admin', ?);
	`
	_, err = db.conn.Exec(sqlStmt, flagKey.PrivateKey, adminJoinToken)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("Failed to create tables: %v", err)
	}

	return db, nil
}

func (db *PersistDatabase) Close() {
	db.conn.Close()
}

func (db *PersistDatabase) exec(query string, values ...interface{}) (int, error) {
	tx, err := db.conn.Begin()
	if err != nil {
		return 0, fmt.Errorf("Failed to begin db transaction: %v", err)
	}
	stmt, err := tx.Prepare(query)
	if err != nil {
		return 0, fmt.Errorf("Failed to prepare db query: %v", err)
	}
	defer stmt.Close()
	result, err := stmt.Exec(values...)
	if err != nil {
		return 0, fmt.Errorf("Failed to execute db query: %v", err)
	}
	err = tx.Commit()
	if err != nil {
		return 0, fmt.Errorf("Failed to commit db stmt: %v", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("Failed to get last inserted row id: %v", err)
	}
	return int(id), nil
}

func (db *PersistDatabase) InsertDevice(device *DeviceConfig) (int, error) {
	id, err := db.exec(
		"insert into devices(user, name, config) values(?, ?, ?)",
		device.UserID, device.Name, device.Config,
	)
	if err != nil {
		return id, err
	}

	device.ID = id
	return id, nil
}

func (db *PersistDatabase) UpdateDeviceConfig(id int, config *string) error {
	_, err := db.exec(
		"update devices set config = ? where id = ?",
		config, id,
	)

	return err
}

func (db *PersistDatabase) InsertUser(user *User) (int, error) {
	id, err := db.exec(
		"insert into users(team, username, password_hash) values(?, ?, ?)",
		user.TeamID, user.Username, user.PasswordHash,
	)
	if err != nil {
		return id, err
	}

	user.ID = id
	return id, nil
}

func (db *PersistDatabase) InsertTeam(team *Team) (int, error) {
	id, err := db.exec(
		"insert into teams(name, join_token) values(?, ?)",
		team.DisplayName, team.JoinToken,
	)
	if err != nil {
		return id, err
	}

	team.ID = id
	return id, nil
}

func (db *PersistDatabase) InsertSession(session *Session) (int, error) {
	id, err := db.exec(
		"insert into sessions(token, user, expires_at) values(?, ?, ?)",
		session.Token, session.UserID, session.ExpiresAt.Unix(),
	)
	if err != nil {
		return id, err
	}

	session.ID = id
	return id, nil
}

func (db *PersistDatabase) queryOne(query string, args ...interface{}) (*sql.Row, error) {
	row := db.conn.QueryRow(query, args...)
	if row.Err() == sql.ErrNoRows {
		return nil, fmt.Errorf("No rows match query: %q", query)
	} else if row.Err() != nil {
		return nil, fmt.Errorf("Failed to query db: %v", row.Err())
	}
	return row, nil
}

type PersistentState struct {
	Tick    int64
	FlagKey ed25519.PrivateKey
}

func (db *PersistDatabase) GetPersistentState() (PersistentState, error) {
	row, err := db.queryOne("select tick, flag_key from state")
	if err != nil {
		return PersistentState{}, err
	}

	var tick int64
	var flagKey []byte
	err = row.Scan(&tick, &flagKey)
	if err != nil {
		return PersistentState{}, err
	}

	return PersistentState{
		Tick:    tick,
		FlagKey: flagKey,
	}, nil
}

func (db *PersistDatabase) UpdatePersistentState(state PersistentState) error {
	_, err := db.exec("update state set tick=?, flag_key=?", state.Tick, state.FlagKey)
	return err
}

func (db *PersistDatabase) ResetPersistentState() error {
	flagKey, err := GenerateKey()
	if err != nil {
		return fmt.Errorf("failed to generate flag signing key: %v", err)
	}

	err = db.UpdatePersistentState(PersistentState{Tick: 1, FlagKey: flagKey.PrivateKey})
	return err
}

func (db *PersistDatabase) UpdateTick(tick int64) error {
	_, err := db.exec("update state set tick=?", tick)
	return err
}

func (db *PersistDatabase) InsertFlagSteal(steal *Steal) (int, error) {
	id, err := db.exec(
		"insert into flag_steals(attacking_team, defending_team, service, steal_tick, flag_tick, time) values(?, ?, ?, ?, ?, ?)",
		steal.AttackingTeamID, steal.Flag.TeamId, steal.Flag.ServiceId, steal.StealTick, steal.StealTime.Unix(),
	)
	if err != nil {
		return id, err
	}

	steal.ID = id
	return id, nil
}

func (db *PersistDatabase) InsertUptimeCheck(check *UptimeCheck) (int, error) {
	id, err := db.exec(
		"insert into uptime_checks(team, service, tick, start_time, duration, success, failure_reason) values(?, ?, ?, ?, ?, ?, ?)",
		check.TeamID, check.ServiceID, check.TickID, check.StartTime.Unix(), check.Duration, check.Success, check.FailureReason,
	)
	if err != nil {
		return id, err
	}

	check.ID = id
	return id, nil
}

func (db *PersistDatabase) GetUserByUsername(username string) (User, error) {
	row, err := db.queryOne("select id, team, password_hash from users where username=?", username)
	if err != nil {
		return User{}, err
	}

	var id int
	var teamID int
	var passwordHash string
	err = row.Scan(&id, &teamID, &passwordHash)
	if err != nil {
		return User{}, err
	}

	return User{
		ID:           id,
		TeamID:       teamID,
		Username:     username,
		PasswordHash: passwordHash,
	}, nil
}

func (db *PersistDatabase) GetSessionByToken(token string) (Session, error) {
	row, err := db.queryOne("select id, user, expires_at from sessions where token=?", token)
	if err != nil {
		return Session{}, fmt.Errorf("Failed to get session by token")
	}

	var id int
	var userID int
	var expiresAt int64
	err = row.Scan(&id, &userID, &expiresAt)
	if err != nil {
		return Session{}, err
	}

	return Session{
		ID:        id,
		Token:     token,
		UserID:    userID,
		ExpiresAt: time.Unix(expiresAt, 0),
	}, nil
}

func (db *PersistDatabase) GetAdminTeam() (Team, error) {
	row, err := db.queryOne("select id, name, join_token from teams where name='admin'")
	if err != nil {
		return Team{}, fmt.Errorf("failed to get admin team: %v", err)
	}

	var id int
	var name string
	var joinToken string
	err = row.Scan(&id, &name, &joinToken)
	if err != nil {
		return Team{}, err
	}

	return Team{ID: id, DisplayName: name, JoinToken: joinToken}, nil
}

func (db *PersistDatabase) GetTeam(id int) (Team, error) {
	row, err := db.queryOne("select name, join_token from teams where id=?", id)
	if err != nil {
		return Team{}, fmt.Errorf("failed to get team by id: %v", err)
	}

	var name string
	var joinToken string
	err = row.Scan(&name, &joinToken)
	if err != nil {
		return Team{}, err
	}

	return Team{ID: id, DisplayName: name, JoinToken: joinToken}, nil
}

func (db *PersistDatabase) DeleteSessionByToken(token string) error {
	_, err := db.exec("delete from sessions where token=?", token)
	if err != nil {
		slog.Error("Failed to get session by token", "err", err)
		return fmt.Errorf("Failed to get session by token")
	}
	return nil
}

func (db *PersistDatabase) RemoveAllFlagSteals() error {
	_, err := db.exec("delete from flag_steals")
	if err != nil {
		slog.Error("Failed to remove all flag steals", "err", err)
		return fmt.Errorf("Failed to remove all flag steals")
	}
	return nil
}

func (db *PersistDatabase) RemoveAllUptimeChecks() error {
	_, err := db.exec("delete from uptime_checks")
	if err != nil {
		slog.Error("Failed to remove all uptime checks", "err", err)
		return fmt.Errorf("Failed to remove all uptime checks")
	}
	return nil
}

func (db *PersistDatabase) queryForEach(query string, cb func(rows *sql.Rows) error) error {
	rows, err := db.conn.Query(query)
	if err != nil {
		return fmt.Errorf("Failed to query db: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		cb(rows)
	}
	err = rows.Err()
	if err != nil {
		return fmt.Errorf("Failed to iterate over query result rows: %v", err)
	}

	return nil
}

func (db *PersistDatabase) ForEachDevice(cb func(device DeviceConfig) error) error {
	return db.queryForEach(
		"select id, user, name, config from devices",
		func(rows *sql.Rows) error {
			var id int
			var userID int
			var name string
			var config *string
			err := rows.Scan(&id, &userID, &name, &config)
			if err != nil {
				return fmt.Errorf("Failed to scan query result row: %v", err)
			}
			if err := cb(DeviceConfig{id, userID, name, config}); err != nil {
				return err
			}
			return nil
		},
	)
}

func (db *PersistDatabase) ForEachUser(cb func(user User) error) error {
	return db.queryForEach(
		"select id, team, username, password_hash from users",
		func(rows *sql.Rows) error {
			var id int
			var team int
			var name string
			var passwordHash string
			err := rows.Scan(&id, &team, &name, &passwordHash)
			if err != nil {
				return fmt.Errorf("Failed to scan query result row: %v", err)
			}
			if err := cb(User{id, team, name, passwordHash}); err != nil {
				return err
			}
			return nil
		},
	)
}

func (db *PersistDatabase) ForEachTeam(cb func(team Team) error) error {
	return db.queryForEach(
		"select id, name, join_token from teams",
		func(rows *sql.Rows) error {
			var id int
			var name string
			var joinToken string
			err := rows.Scan(&id, &name, &joinToken)
			if err != nil {
				return fmt.Errorf("Failed to scan query result row: %v", err)
			}
			if err := cb(Team{id, name, joinToken}); err != nil {
				return err
			}
			return nil
		},
	)
}

func (db *PersistDatabase) ForEachInstance(cb func(user Instance) error) error {
	return db.queryForEach(
		"select id, team, name, ssh_config from instances",
		func(rows *sql.Rows) error {
			var id int
			var team int
			var name string
			var sshConfig string
			err := rows.Scan(&id, &team, &name, &sshConfig)
			if err != nil {
				return fmt.Errorf("Failed to scan query result row: %v", err)
			}
			if err := cb(Instance{id, team, name, sshConfig}); err != nil {
				return err
			}
			return nil
		},
	)
}

func (db *PersistDatabase) ForEachFlagSteal(cb func(steal Steal) error) error {
	return db.queryForEach(
		"select id, attacking_team, defending_team, service, steal_tick, flag_tick, time from flag_steals",
		func(rows *sql.Rows) error {
			var id int
			var attackingTeam int
			var defendingTeam int
			var service int
			var stealTick int
			var flagTick int
			var stealTime int64
			err := rows.Scan(&id, &attackingTeam, &defendingTeam, &service, &stealTick, &flagTick, &stealTime)
			if err != nil {
				return fmt.Errorf("Failed to scan query result row: %v", err)
			}

			flag := FlagInfo{TeamId: defendingTeam, TickId: flagTick, ServiceId: service}
			if err := cb(Steal{
				ID:              id,
				Flag:            flag,
				AttackingTeamID: attackingTeam,
				StealTick:       stealTick,
				StealTime:       time.Unix(stealTime, 0),
			}); err != nil {
				return err
			}
			return nil
		},
	)
}

func (db *PersistDatabase) ForEachUptimeCheck(cb func(check UptimeCheck) error) error {
	return db.queryForEach(
		"select id, team, service, tick, start_time, duration, success, failure_reason from uptime_checks",
		func(rows *sql.Rows) error {
			var id int
			var team int
			var service int
			var tick int
			var startTime int64
			var duration float64
			var success bool
			var failureReason *string
			err := rows.Scan(&id, &team, &service, &tick, &startTime, &duration, &success, &failureReason)
			if err != nil {
				return fmt.Errorf("Failed to scan query result row: %v", err)
			}

			if err := cb(UptimeCheck{
				ID:            id,
				TeamID:        team,
				ServiceID:     service,
				TickID:        tick,
				StartTime:     time.Unix(startTime, 0),
				Duration:      duration,
				Success:       success,
				FailureReason: failureReason,
			}); err != nil {
				return err
			}
			return nil
		},
	)
}

func (db *PersistDatabase) GetUser(id int) (User, error) {
	row, err := db.queryOne("select team, username, password_hash from users where id == ?", id)
	if err != nil {
		return User{}, err
	}

	var team int
	var name string
	var passwordHash string
	err = row.Scan(&team, &name, &passwordHash)
	if err != nil {
		return User{}, fmt.Errorf("Failed to scan query result row: %v", err)
	}
	return User{id, team, name, passwordHash}, nil
}
