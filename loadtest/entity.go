// Copyright (c) 2016 Mattermost, Inc. All Rights Reserved.
// See License.txt for license information.

package loadtest

import (
	"math/rand"
	"runtime/debug"
	"sync"
	"time"

	"github.com/pkg/errors"

	"github.com/mattermost/mattermost-load-test/cmdlog"
	"github.com/mattermost/mattermost-load-test/randutil"
	"github.com/mattermost/mattermost-server/model"
)

type EntityConfig struct {
	EntityNumber        int
	EntityName          string
	EntityActions       []randutil.Choice
	UserData            UserImportData
	ChannelMap          map[string]string
	TeamMap             map[string]string
	TownSquareMap       map[string]string
	Client              *model.Client4
	AdminClient         *model.Client4
	WebSocketClient     *model.WebSocketClient
	ActionRate          time.Duration
	LoadTestConfig      *LoadTestConfig
	StatusReportChannel chan<- UserEntityStatusReport
	StopChannel         <-chan bool
	StopWaitGroup       *sync.WaitGroup
	Info                map[string]interface{}
}

func (ec *EntityConfig) Initialize() error {
	user, response := ec.Client.GetMe("")
	if response.Error != nil {
		return errors.Wrap(response.Error, "failed to fetch current user")
	}

	teams, response := ec.Client.GetTeamsForUser(user.Id, "")
	if response.Error != nil {
		return errors.Wrapf(response.Error, "failed to fetch teams for user %s", user.Id)
	}

	teamData := []UserTeamImportData{}
	teamChoice := []randutil.Choice{}
	for i, team := range teams {
		teamChannels, response := ec.Client.GetChannelsForTeamForUser(team.Id, user.Id, "")
		if response.Error != nil {
			return errors.Wrapf(response.Error, "failed to fetch user channels for team %s", team.Id)
		}

		teamChannelsData := []UserChannelImportData{}
		channelChoice := []randutil.Choice{}
		for i, channel := range teamChannels {
			teamChannelsData = append(teamChannelsData, UserChannelImportData{
				Name: channel.Name,
				// Roles
			})
			channelChoice = append(channelChoice, randutil.Choice{
				Item: i,
				// TODO: Weighted channels
				Weight: 1,
			})
		}

		teamData = append(teamData, UserTeamImportData{
			Name: team.Name,
			// Roles         string                  `json:"roles"`
			Channels:      teamChannelsData,
			ChannelChoice: channelChoice,
		})
		teamChoice = append(teamChoice, randutil.Choice{
			Item: i,
			// TODO: Weighted channels
			Weight: 1,
		})
	}

	ec.UserData = UserImportData{
		Username:    user.Username,
		Email:       user.Email,
		AuthService: user.AuthService,
		AuthData:    pToS(user.AuthData),
		Password:    user.Password,
		Nickname:    user.Nickname,
		FirstName:   user.FirstName,
		LastName:    user.LastName,
		Position:    user.Position,
		Roles:       user.Roles,
		Locale:      user.Locale,

		Teams:      teamData,
		TeamChoice: teamChoice,

		// Theme              string `json:"theme,omitempty"`
		// SelectedFont       string `json:"display_font,omitempty"`
		// UseMilitaryTime    string `json:"military_time,omitempty"`
		// NameFormat         string `json:"teammate_name_display,omitempty"`
		// CollapsePreviews   string `json:"link_previews,omitempty"`
		// MessageDisplay     string `json:"message_display,omitempty"`
		// ChannelDisplayMode string `json:"channel_display_mode,omitempty"`
	}

	return nil
}

func runEntity(ec *EntityConfig) {
	defer func() {
		if r := recover(); r != nil {
			cmdlog.Errorf("Recovered: %s: %s", r, debug.Stack())
			ec.StopWaitGroup.Add(1)
			go runEntity(ec)
		}
	}()
	defer ec.StopWaitGroup.Done()

	actionRateMaxVarianceMilliseconds := ec.LoadTestConfig.UserEntitiesConfiguration.ActionRateMaxVarianceMilliseconds

	// Ensure that the entities act at uniformly distributed times.
	now := time.Now()
	intervalStart := time.Unix(0, now.UnixNano()-now.UnixNano()%int64(ec.ActionRate/time.Nanosecond))
	start := intervalStart.Add(time.Duration(rand.Int63n(int64(ec.ActionRate))))
	if start.Before(now) {
		start = start.Add(ec.ActionRate)
	}
	delay := start.Sub(now)

	timer := time.NewTimer(delay)
	for {
		select {
		case <-ec.StopChannel:
			return
		case <-timer.C:
			action, err := randutil.WeightedChoice(ec.EntityActions)
			if err != nil {
				cmdlog.Error("Failed to pick weighted choice")
				return
			}
			action.Item.(func(*EntityConfig))(ec)
			halfVarianceDuration := time.Duration(actionRateMaxVarianceMilliseconds / 2.0)
			randomDurationWithinVariance := time.Duration(rand.Intn(actionRateMaxVarianceMilliseconds))
			timer.Reset(ec.ActionRate + randomDurationWithinVariance - halfVarianceDuration)
		}
	}
}

func doStatusPolling(ec *EntityConfig) {
	defer func() {
		if r := recover(); r != nil {
			cmdlog.Errorf("%s: %s", r, debug.Stack())
			ec.StopWaitGroup.Add(1)
			go doStatusPolling(ec)
		}
	}()
	defer ec.StopWaitGroup.Done()

	ticker := time.NewTicker(60 * time.Second)
	for {
		select {
		case <-ec.StopChannel:
			return
		case <-ticker.C:
			actionGetStatuses(ec)
		}
	}
}

func websocketListen(ec *EntityConfig) {
	defer ec.StopWaitGroup.Done()

	if ec.WebSocketClient == nil {
		return
	}

	ec.WebSocketClient.Listen()

	websocketRetryCount := 0

	for {
		select {
		case <-ec.StopChannel:
			return
		case _, ok := <-ec.WebSocketClient.EventChannel:
			if !ok {
				// If we are set to retry connection, first retry immediately, then backoff until retry max is reached
				for {
					if websocketRetryCount > 5 {
						if ec.WebSocketClient.ListenError != nil {
							cmdlog.Errorf("Websocket Error: %v", ec.WebSocketClient.ListenError.Error())
						} else {
							cmdlog.Error("Server closed websocket")
						}
						cmdlog.Error("Websocket disconneced. Max retries reached.")
						return
					}
					time.Sleep(time.Duration(websocketRetryCount) * time.Second)
					if err := ec.WebSocketClient.Connect(); err != nil {
						websocketRetryCount++
						continue
					}
					ec.WebSocketClient.Listen()
					break
				}
			}
		}
	}
}

func (config *EntityConfig) SendStatus(status int, err error, details string) {
	config.StatusReportChannel <- UserEntityStatusReport{
		Status:  status,
		Err:     err,
		Config:  config,
		Details: details,
	}
}

func (config *EntityConfig) SendStatusLaunching() {
	config.SendStatus(STATUS_LAUNCHING, nil, "")
}

func (config *EntityConfig) SendStatusActive(details string) {
	config.SendStatus(STATUS_ACTIVE, nil, details)
}

func (config *EntityConfig) SendStatusError(err error, details string) {
	config.SendStatus(STATUS_ERROR, err, details)
}

func (config *EntityConfig) SendStatusFailedLaunch(err error, details string) {
	config.SendStatus(STATUS_FAILED_LAUNCH, err, details)
}

func (config *EntityConfig) SendStatusFailedActive(err error, details string) {
	config.SendStatus(STATUS_FAILED_ACTIVE, err, details)
}

func (config *EntityConfig) SendStatusActionSend(details string) {
	config.SendStatus(STATUS_ACTION_SEND, nil, details)
}

func (config *EntityConfig) SendStatusActionRecieve(details string) {
	config.SendStatus(STATUS_ACTION_RECIEVE, nil, details)
}

func (config *EntityConfig) SendStatusStopped(details string) {
	config.SendStatus(STATUS_STOPPED, nil, details)
}

func pToS(s *string) string {
	if s == nil {
		return ""
	}

	return *s
}
