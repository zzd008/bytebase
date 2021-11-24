package api

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/robfig/cron/v3"
)

// PolicyType is the type or name of a policy.
type PolicyType string

// PipelineApprovalValue is value for approval policy.
type PipelineApprovalValue string

// BackupPlanPolicySchedule is value for backup plan policy.
type BackupPlanPolicySchedule string

// WindowCron is value for allowed window
//policy, and is stored in cron format.
// a valid cron expression should be parsed without any error by this: cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
type WindowCron string

const (
	// PolicyTypePipelineApproval is the approval policy type.
	PolicyTypePipelineApproval PolicyType = "bb.policy.pipeline-approval"
	// PolicyTypeBackupPlan is the backup plan policy type.
	PolicyTypeBackupPlan PolicyType = "bb.policy.backup-plan"
	// PolicyTypeWindow is the allowed window type and is stored in cron format
	PolicyTypeWindow PolicyType = "bb.policy.window"

	// PipelineApprovalValueManualNever is MANUAL_APPROVAL_NEVER approval policy value.
	PipelineApprovalValueManualNever PipelineApprovalValue = "MANUAL_APPROVAL_NEVER"
	// PipelineApprovalValueManualAlways is MANUAL_APPROVAL_ALWAYS approval policy value.
	PipelineApprovalValueManualAlways PipelineApprovalValue = "MANUAL_APPROVAL_ALWAYS"

	// BackupPlanPolicyScheduleUnset is NEVER backup plan policy value.
	BackupPlanPolicyScheduleUnset BackupPlanPolicySchedule = "UNSET"
	// BackupPlanPolicyScheduleDaily is DAILY backup plan policy value.
	BackupPlanPolicyScheduleDaily BackupPlanPolicySchedule = "DAILY"
	// BackupPlanPolicyScheduleWeekly is WEEKLY backup plan policy value.
	BackupPlanPolicyScheduleWeekly BackupPlanPolicySchedule = "WEEKLY"

	// WindowUnset is ANYTIME allowed window cron.
	WindowCronUnset WindowCron = ""
)

var (
	// PolicyTypes is a set of all policy types.
	PolicyTypes = map[PolicyType]bool{
		PolicyTypePipelineApproval: true,
		PolicyTypeBackupPlan:       true,
		PolicyTypeWindow:           true,
	}
)

type Policy struct {
	ID int `jsonapi:"primary,policy"`

	// Standard fields
	RowStatus RowStatus `jsonapi:"attr,rowStatus"`
	CreatorId int
	Creator   *Principal `jsonapi:"attr,creator"`
	CreatedTs int64      `jsonapi:"attr,createdTs"`
	UpdaterId int
	Updater   *Principal `jsonapi:"attr,updater"`
	UpdatedTs int64      `jsonapi:"attr,updatedTs"`

	// Related fields
	EnvironmentId int
	Environment   *Environment `jsonapi:"relation,environment"`

	// Domain specific fields
	Type    PolicyType `jsonapi:"attr,type"`
	Payload string     `jsonapi:"attr,payload"`
}

// PolicyFind is the message to get a policy.
type PolicyFind struct {
	ID *int

	// Related fields
	EnvironmentId *int

	// Domain specific fields
	Type *PolicyType `jsonapi:"attr,type"`
}

// PolicyUpsert is the message to upsert a policy.
// NOTE: We use PATCH for Upsert, this is inspired by https://google.aip.dev/134#patch-and-put
type PolicyUpsert struct {
	// Standard fields
	// Value is assigned from the jwt subject field passed by the client.
	// CreatorId is the ID of the creator.
	UpdaterId int

	// Related fields
	EnvironmentId int

	// Domain specific fields
	Type    PolicyType
	Payload string `jsonapi:"attr,payload"`
}

// PolicyService is the backend for policies.
type PolicyService interface {
	FindPolicy(ctx context.Context, find *PolicyFind) (*Policy, error)
	UpsertPolicy(ctx context.Context, upsert *PolicyUpsert) (*Policy, error)
	GetBackupPlanPolicy(ctx context.Context, environmentID int) (*BackupPlanPolicy, error)
	GetPipelineApprovalPolicy(ctx context.Context, environmentID int) (*PipelineApprovalPolicy, error)
}

// PipelineApprovalPolicy is the policy configuration for pipeline approval
type PipelineApprovalPolicy struct {
	Value PipelineApprovalValue `json:"value"`
}

func (pa PipelineApprovalPolicy) String() (string, error) {
	s, err := json.Marshal(pa)
	if err != nil {
		return "", err
	}
	return string(s), nil
}

// UnmarshalPipelineApprovalPolicy will unmarshal payload to pipeline approval policy.
func UnmarshalPipelineApprovalPolicy(payload string) (*PipelineApprovalPolicy, error) {
	var pa PipelineApprovalPolicy
	if err := json.Unmarshal([]byte(payload), &pa); err != nil {
		return nil, fmt.Errorf("failed to unmarshal pipeline approval policy %q: %q", payload, err)
	}
	return &pa, nil
}

// BackupPlanPolicy is the policy configuration for backup plan.
type BackupPlanPolicy struct {
	Schedule BackupPlanPolicySchedule `json:"schedule"`
}

func (bp BackupPlanPolicy) String() (string, error) {
	s, err := json.Marshal(bp)
	if err != nil {
		return "", err
	}
	return string(s), nil
}

// UnmarshalBackupPlanPolicy will unmarshal payload to back up plan policy.
func UnmarshalBackupPlanPolicy(payload string) (*BackupPlanPolicy, error) {
	var bp BackupPlanPolicy
	if err := json.Unmarshal([]byte(payload), &bp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal backup plan policy %q: %q", payload, err)
	}
	return &bp, nil
}

// WindowType specify the window type
type WindowType int

const (
	WindowTypeAllow WindowType = iota
	WindowTypeDeny
	WindowTypeUnknown
)

// WindowPolicy is the policy configuration for window.
type WindowPolicy struct {
	WindowType WindowType `json:"windowType"`
	WindowCron WindowCron `json:"windowCron"`
}

func GetWindowCronParser() *cron.Parser {
	cronParser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	return &cronParser
}

func (ap WindowPolicy) String() (string, error) {
	s, err := json.Marshal(ap)
	if err != nil {
		return "", err
	}
	return string(s), nil
}

// UnmarshalWindowPolicy will unmarshal payload to allowed window policy.
func UnmarshalWindowPolicy(payload string) (*WindowPolicy, error) {
	var wp WindowPolicy
	if err := json.Unmarshal([]byte(payload), &wp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal window policy %q: %q", payload, err)
	}
	return &wp, nil
}

// ValidatePolicy will validate the policy type and payload values.
func ValidatePolicy(pType PolicyType, payload string) error {
	if !PolicyTypes[pType] {
		return fmt.Errorf("invalid policy type: %s", pType)
	}
	if payload == "" {
		return nil
	}

	switch pType {
	case PolicyTypePipelineApproval:
		pa, err := UnmarshalPipelineApprovalPolicy(payload)
		if err != nil {
			return err
		}
		if pa.Value != PipelineApprovalValueManualNever && pa.Value != PipelineApprovalValueManualAlways {
			return fmt.Errorf("invalid approval policy value: %q", payload)
		}
	case PolicyTypeBackupPlan:
		bp, err := UnmarshalBackupPlanPolicy(payload)
		if err != nil {
			return err
		}
		if bp.Schedule != BackupPlanPolicyScheduleUnset && bp.Schedule != BackupPlanPolicyScheduleDaily && bp.Schedule != BackupPlanPolicyScheduleWeekly {
			return fmt.Errorf("invalid backup plan policy schedule: %q", bp.Schedule)
		}
	case PolicyTypeWindow:
		ap, err := UnmarshalWindowPolicy(payload)
		if err != nil {
			return err
		}
		parser := GetWindowCronParser()
		_, err = parser.Parse(string(ap.WindowCron))
		if err != nil {
			return err
		}
	}

	return nil
}

// GetDefaultPolicy will return the default value for the given policy type.
// The default policy can be empty when we don't have anything to enforce at runtime.
func GetDefaultPolicy(pType PolicyType) (string, error) {
	switch pType {
	case PolicyTypePipelineApproval:
		return PipelineApprovalPolicy{
			Value: PipelineApprovalValueManualAlways,
		}.String()
	case PolicyTypeBackupPlan:
		return BackupPlanPolicy{
			Schedule: BackupPlanPolicyScheduleUnset,
		}.String()
	case PolicyTypeWindow:
		return WindowPolicy{
			WindowType: WindowTypeUnknown,
			WindowCron: WindowCronUnset,
		}.String()
	}

	return "", nil
}
