// mautrix-olx - A Matrix-OLX puppeting bridge.
// Copyright (C) 2026 Maximilian Gaedig
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

package connector

import (
	_ "embed"
	"fmt"
	"strings"
	"text/template"
	"time"

	up "go.mau.fi/util/configupgrade"
	"gopkg.in/yaml.v3"
	"maunium.net/go/mautrix/event"
)

//go:embed example-config.yaml
var ExampleConfig string

type PresenceConfig struct {
	Enabled      bool          `yaml:"enabled"`
	PollInterval time.Duration `yaml:"poll_interval"`
	MaxUsers     int           `yaml:"max_users"`
}

type SyncConfig struct {
	Archived bool          `yaml:"archived"`
	Interval time.Duration `yaml:"interval"`
}

type Config struct {
	DisplaynameTemplate string `yaml:"displayname_template"`
	RoomNameTemplate    string `yaml:"room_name_template"`

	UserAgent     string `yaml:"user_agent"`
	ClientVersion string `yaml:"client_version"`
	Proxy         string `yaml:"proxy"`
	WWWProxy      string `yaml:"www_proxy"`

	Presence PresenceConfig `yaml:"presence"`
	Sync     SyncConfig     `yaml:"sync"`

	ArchiveTag            event.RoomTag `yaml:"archive_tag"`
	SavedTag              event.RoomTag `yaml:"saved_tag"`
	DeleteChatPermanently bool          `yaml:"delete_chat_permanently"`

	displaynameTemplate *template.Template `yaml:"-"`
	roomNameTemplate    *template.Template `yaml:"-"`
}

type umConfig Config

func (c *Config) UnmarshalYAML(node *yaml.Node) error {
	err := node.Decode((*umConfig)(c))
	if err != nil {
		return err
	}
	return c.PostProcess()
}

func (c *Config) PostProcess() (err error) {
	c.displaynameTemplate, err = template.New("displayname").Parse(c.DisplaynameTemplate)
	if err != nil {
		return fmt.Errorf("invalid displayname_template: %w", err)
	}
	c.roomNameTemplate, err = template.New("room_name").Parse(c.RoomNameTemplate)
	if err != nil {
		return fmt.Errorf("invalid room_name_template: %w", err)
	}
	return nil
}

type DisplaynameParams struct {
	Name     string
	Business bool
}

func (c *Config) FormatDisplayname(params DisplaynameParams) string {
	if strings.TrimSpace(params.Name) == "" {
		params.Name = "OLX user"
	}
	if c.displaynameTemplate == nil {
		return params.Name
	}
	var buf strings.Builder
	if err := c.displaynameTemplate.Execute(&buf, params); err != nil {
		return params.Name
	}
	return strings.TrimSpace(buf.String())
}

type RoomNameParams struct {
	Name  string
	Title string
	Price string
}

func (c *Config) FormatRoomName(params RoomNameParams) string {
	if strings.TrimSpace(params.Name) == "" {
		params.Name = "OLX user"
	}
	fallback := params.Name
	if params.Title != "" {
		fallback += " · " + params.Title
	}
	if c.roomNameTemplate == nil {
		return fallback
	}
	var buf strings.Builder
	if err := c.roomNameTemplate.Execute(&buf, params); err != nil {
		return fallback
	}
	// A chat without an ad title must not end in a dangling separator.
	return strings.Trim(strings.TrimSpace(buf.String()), "·-–—| ")
}

func upgradeConfig(helper up.Helper) {
	helper.Copy(up.Str, "displayname_template")
	helper.Copy(up.Str, "room_name_template")
	helper.Copy(up.Str|up.Null, "user_agent")
	helper.Copy(up.Str, "client_version")
	helper.Copy(up.Str|up.Null, "proxy")
	helper.Copy(up.Str|up.Null, "www_proxy")
	helper.Copy(up.Bool, "presence", "enabled")
	helper.Copy(up.Str, "presence", "poll_interval")
	helper.Copy(up.Int, "presence", "max_users")
	helper.Copy(up.Bool, "sync", "archived")
	helper.Copy(up.Str, "sync", "interval")
	helper.Copy(up.Str|up.Null, "archive_tag")
	helper.Copy(up.Str|up.Null, "saved_tag")
	helper.Copy(up.Bool, "delete_chat_permanently")
}

func (oc *OLXConnector) GetConfig() (string, any, up.Upgrader) {
	return ExampleConfig, &oc.Config, &up.StructUpgrader{
		SimpleUpgrader: up.SimpleUpgrader(upgradeConfig),
		Blocks: [][]string{
			{"user_agent"},
			{"presence"},
			{"sync"},
			{"archive_tag"},
		},
		Base: ExampleConfig,
	}
}
