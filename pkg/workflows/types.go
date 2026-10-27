package workflows

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

type Workflow struct {
	Name  string           `json:"name,omitempty"  yaml:"name,omitempty"`
	On    WorkflowTriggers `json:"on,omitempty"    yaml:"on,omitempty"`
	Gates map[string]any   `json:"gates,omitempty" yaml:"gates,omitempty"`
	Jobs  map[string]Job   `json:"jobs"            yaml:"jobs"`
}

type WorkflowTriggers struct {
	Manual         map[string]any         `json:"manual,omitempty"          yaml:"manual,omitempty"`
	Schedule       []ScheduleTrigger      `json:"schedule,omitempty"        yaml:"schedule,omitempty"`
	ChannelMessage *ChannelMessageTrigger `json:"channel_message,omitempty" yaml:"channel_message,omitempty"`
	Command        *CommandTrigger        `json:"command,omitempty"         yaml:"command,omitempty"`
	RuntimeEvent   *RuntimeEventTrigger   `json:"runtime_event,omitempty"   yaml:"runtime_event,omitempty"`
	Event          *EventTrigger          `json:"event,omitempty"           yaml:"event,omitempty"`
	WorkflowCall   *WorkflowCall          `json:"workflow_call,omitempty"   yaml:"workflow_call,omitempty"`
}

type ScheduleTrigger struct {
	Cron string `json:"cron,omitempty" yaml:"cron,omitempty"`
}

type ChannelMessageTrigger struct {
	Channels     StringList       `json:"channels,omitempty"     yaml:"channels,omitempty"`
	Chats        StringList       `json:"chats,omitempty"        yaml:"chats,omitempty"`
	Senders      StringList       `json:"senders,omitempty"      yaml:"senders,omitempty"`
	Mentioned    *bool            `json:"mentioned,omitempty"    yaml:"mentioned,omitempty"`
	Command      string           `json:"command,omitempty"      yaml:"command,omitempty"`
	TextMatches  string           `json:"text_matches,omitempty" yaml:"text_matches,omitempty"`
	Passthrough  *bool            `json:"passthrough,omitempty"  yaml:"passthrough,omitempty"`
	Conversation ConversationSpec `json:"conversation,omitempty" yaml:"conversation,omitempty"`
}

type CommandTrigger struct {
	Name         string           `json:"name,omitempty"         yaml:"name,omitempty"`
	Channels     StringList       `json:"channels,omitempty"     yaml:"channels,omitempty"`
	Chats        StringList       `json:"chats,omitempty"        yaml:"chats,omitempty"`
	Senders      StringList       `json:"senders,omitempty"      yaml:"senders,omitempty"`
	Args         map[string]Input `json:"args,omitempty"         yaml:"args,omitempty"`
	Passthrough  *bool            `json:"passthrough,omitempty"  yaml:"passthrough,omitempty"`
	Conversation ConversationSpec `json:"conversation,omitempty" yaml:"conversation,omitempty"`
}

type RuntimeEventTrigger struct {
	Kinds    StringList `json:"kinds,omitempty"    yaml:"kinds,omitempty"`
	Sources  StringList `json:"sources,omitempty"  yaml:"sources,omitempty"`
	Agents   StringList `json:"agents,omitempty"   yaml:"agents,omitempty"`
	Sessions StringList `json:"sessions,omitempty" yaml:"sessions,omitempty"`
	Channels StringList `json:"channels,omitempty" yaml:"channels,omitempty"`
	Chats    StringList `json:"chats,omitempty"    yaml:"chats,omitempty"`
}

// EventTrigger filters connector-independent events from the durable external
// event inbox. Values in each list are alternatives; populated fields are
// combined.
type EventTrigger struct {
	Sources    StringList            `json:"sources,omitempty"    yaml:"sources,omitempty"`
	Connectors StringList            `json:"connectors,omitempty" yaml:"connectors,omitempty"`
	Types      StringList            `json:"types,omitempty"      yaml:"types,omitempty"`
	Actor      *EventEntityTrigger   `json:"actor,omitempty"      yaml:"actor,omitempty"`
	Subject    *EventEntityTrigger   `json:"subject,omitempty"    yaml:"subject,omitempty"`
	Attributes map[string]StringList `json:"attributes,omitempty" yaml:"attributes,omitempty"`
}

// EventEntityTrigger filters the normalized actor or subject attached to an
// external event.
type EventEntityTrigger struct {
	IDs        StringList            `json:"ids,omitempty"        yaml:"ids,omitempty"`
	Types      StringList            `json:"types,omitempty"      yaml:"types,omitempty"`
	Attributes map[string]StringList `json:"attributes,omitempty" yaml:"attributes,omitempty"`
}

type WorkflowCall struct {
	Inputs  map[string]Input  `json:"inputs,omitempty"  yaml:"inputs,omitempty"`
	Secrets map[string]Secret `json:"secrets,omitempty" yaml:"secrets,omitempty"`
	Outputs map[string]Output `json:"outputs,omitempty" yaml:"outputs,omitempty"`
}

type Input struct {
	Type     string `json:"type,omitempty"     yaml:"type,omitempty"`
	Required bool   `json:"required,omitempty" yaml:"required,omitempty"`
	Default  any    `json:"default,omitempty"  yaml:"default,omitempty"`
}

type Secret struct {
	Required bool `json:"required,omitempty" yaml:"required,omitempty"`
}

type Output struct {
	Value string `json:"value,omitempty" yaml:"value,omitempty"`
}

type Repeat struct {
	MaxAttempts int    `json:"max-attempts" yaml:"max-attempts"`
	Until       string `json:"until" yaml:"until"`
}

// ForEach executes a reusable workflow sequentially over a pinned array.
// Key is evaluated against each item; payloads remain data.
type ForEach struct {
	Items any    `json:"items" yaml:"items"`
	Key   string `json:"key" yaml:"key"`
}

type Job struct {
	ForEach         *ForEach          `json:"for-each,omitempty" yaml:"for-each,omitempty"`
	Repeat          *Repeat           `json:"repeat,omitempty" yaml:"repeat,omitempty"`
	Name            string            `json:"name,omitempty"              yaml:"name,omitempty"`
	RunsOn          string            `json:"runs-on,omitempty"           yaml:"runs-on,omitempty"`
	Needs           StringList        `json:"needs,omitempty"             yaml:"needs,omitempty"`
	Uses            string            `json:"uses,omitempty"              yaml:"uses,omitempty"`
	If              string            `json:"if,omitempty"                yaml:"if,omitempty"`
	ContinueOnError bool              `json:"continue_on_error,omitempty" yaml:"continue-on-error,omitempty"`
	With            map[string]any    `json:"with,omitempty"              yaml:"with,omitempty"`
	Secrets         any               `json:"secrets,omitempty"           yaml:"secrets,omitempty"`
	Outputs         map[string]string `json:"outputs,omitempty"           yaml:"outputs,omitempty"`
	Steps           []Step            `json:"steps,omitempty"             yaml:"steps,omitempty"`
	Context         RunContext        `json:"context,omitempty"           yaml:"context,omitempty"`
}

type Step struct {
	Argv             []string       `json:"argv,omitempty" yaml:"argv,omitempty"`
	Run              string         `json:"run,omitempty" yaml:"run,omitempty"`
	Shell            string         `json:"shell,omitempty" yaml:"shell,omitempty"`
	WorkingDirectory string         `json:"working-directory,omitempty" yaml:"working-directory,omitempty"`
	Timeout          string         `json:"timeout,omitempty" yaml:"timeout,omitempty"`
	ID               string         `json:"id,omitempty"                yaml:"id,omitempty"`
	Name             string         `json:"name,omitempty"              yaml:"name,omitempty"`
	Uses             string         `json:"uses,omitempty"              yaml:"uses,omitempty"`
	If               string         `json:"if,omitempty"                yaml:"if,omitempty"`
	ContinueOnError  bool           `json:"continue_on_error,omitempty" yaml:"continue-on-error,omitempty"`
	With             map[string]any `json:"with,omitempty"              yaml:"with,omitempty"`
	Context          RunContext     `json:"context,omitempty"           yaml:"context,omitempty"`
}

type ConversationSpec struct {
	Session  string `json:"session,omitempty"  yaml:"session,omitempty"`
	Delivery string `json:"delivery,omitempty" yaml:"delivery,omitempty"`
}

type RunContext struct {
	Session  string `json:"session,omitempty"  yaml:"session,omitempty"`
	Delivery string `json:"delivery,omitempty" yaml:"delivery,omitempty"`
}

type StringList []string

func (w *Workflow) UnmarshalYAML(value *yaml.Node) error {
	if value == nil || value.Kind == 0 {
		return nil
	}
	if value.Kind != yaml.MappingNode {
		return fmt.Errorf("workflow must be a mapping")
	}
	for i := 0; i+1 < len(value.Content); i += 2 {
		key := strings.TrimSpace(value.Content[i].Value)
		val := value.Content[i+1]
		switch key {
		case "name":
			if err := val.Decode(&w.Name); err != nil {
				return err
			}
		case "on", "true":
			if err := val.Decode(&w.On); err != nil {
				return err
			}
		case "gates":
			if err := val.Decode(&w.Gates); err != nil {
				return err
			}
		case "jobs":
			if err := val.Decode(&w.Jobs); err != nil {
				return err
			}
		}
	}
	return nil
}

func (t *WorkflowTriggers) UnmarshalYAML(value *yaml.Node) error {
	if value == nil || value.Kind == 0 {
		return nil
	}
	if value.Kind != yaml.MappingNode {
		return fmt.Errorf("on must be a mapping")
	}
	type rawTriggers WorkflowTriggers
	var raw rawTriggers
	if err := value.Decode(&raw); err != nil {
		return err
	}
	*t = WorkflowTriggers(raw)
	return nil
}

func (s *StringList) UnmarshalYAML(value *yaml.Node) error {
	if value == nil || value.Kind == 0 {
		return nil
	}
	switch value.Kind {
	case yaml.ScalarNode:
		var item string
		if err := value.Decode(&item); err != nil {
			return err
		}
		item = strings.TrimSpace(item)
		if item == "" {
			*s = nil
			return nil
		}
		*s = StringList{item}
		return nil
	case yaml.SequenceNode:
		var items []string
		if err := value.Decode(&items); err != nil {
			return err
		}
		out := make([]string, 0, len(items))
		for _, item := range items {
			item = strings.TrimSpace(item)
			if item != "" {
				out = append(out, item)
			}
		}
		*s = out
		return nil
	default:
		return fmt.Errorf("expected string or string list")
	}
}
