package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/bytebase/bytebase/api"
	"github.com/bytebase/bytebase/common"
	"github.com/bytebase/bytebase/plugin/db"
	"go.uber.org/zap"
)

const (
	ANOMALY_SCAN_INTERVAL = time.Duration(30) * time.Minute
)

func NewAnomalyScanner(logger *zap.Logger, server *Server) *AnomalyScanner {
	return &AnomalyScanner{
		l:      logger,
		server: server,
	}
}

type AnomalyScanner struct {
	l      *zap.Logger
	server *Server
}

func (s *AnomalyScanner) Run() error {
	go func() {
		s.l.Debug(fmt.Sprintf("Anomaly scanner started and will run every %v", ANOMALY_SCAN_INTERVAL))
		runningTasks := make(map[int]bool)
		mu := sync.RWMutex{}
		for {
			s.l.Debug("New anomaly scanner round started...")
			func() {
				defer func() {
					if r := recover(); r != nil {
						err, ok := r.(error)
						if !ok {
							err = fmt.Errorf("%v", r)
						}
						s.l.Error("Anomaly scanner PANIC RECOVER", zap.Error(err))
					}
				}()

				environmentFind := &api.EnvironmentFind{}
				environmentList, err := s.server.EnvironmentService.FindEnvironmentList(context.Background(), environmentFind)
				if err != nil {
					s.l.Error("Failed to retrieve instance list", zap.Error(err))
					return
				}

				backupPlanPolicyMap := make(map[int]*api.BackupPlanPolicy)
				for _, env := range environmentList {
					policy, err := s.server.PolicyService.GetBackupPlanPolicy(context.Background(), env.ID)
					if err != nil {
						s.l.Error("Failed to retrieve backup policy",
							zap.String("environment", env.Name),
							zap.Error(err))
						return
					}
					backupPlanPolicyMap[env.ID] = policy
				}

				rowStatus := api.Normal
				instanceFind := &api.InstanceFind{
					RowStatus: &rowStatus,
				}
				instanceList, err := s.server.InstanceService.FindInstanceList(context.Background(), instanceFind)
				if err != nil {
					s.l.Error("Failed to retrieve instance list", zap.Error(err))
					return
				}

				for _, instance := range instanceList {
					for _, env := range environmentList {
						if env.ID == instance.EnvironmentId {
							if env.RowStatus == api.Normal {
								instance.Environment = env
							}
							break
						}
					}

					if err := s.server.ComposeInstanceAdminDataSource(context.Background(), instance); err != nil {
						s.l.Error("Failed to retrieve instance admin connection info",
							zap.String("instance", instance.Name),
							zap.Error(err))
						return
					}

					if instance.Environment == nil {
						continue
					}

					mu.Lock()
					if _, ok := runningTasks[instance.ID]; ok {
						mu.Unlock()
						continue
					}
					runningTasks[instance.ID] = true
					mu.Unlock()

					// Do NOT use go-routine otherwise would cause "database locked" in underlying SQLite
					func(instance *api.Instance) {
						s.l.Debug("Scan instance anomaly", zap.String("instance", instance.Name))
						defer func() {
							mu.Lock()
							delete(runningTasks, instance.ID)
							mu.Unlock()
						}()

						databaseFind := &api.DatabaseFind{
							InstanceId: &instance.ID,
						}
						dbList, err := s.server.DatabaseService.FindDatabaseList(context.Background(), databaseFind)
						if err != nil {
							s.l.Error("Failed to retrieve database list",
								zap.String("instance", instance.Name),
								zap.Error(err))
							return
						}
						for _, database := range dbList {
							s.checkDatabaseAnomaly(context.Background(), instance, database)
							s.checkBackupAnomaly(context.Background(), instance, database, backupPlanPolicyMap)
						}
					}(instance)
				}
			}()

			time.Sleep(ANOMALY_SCAN_INTERVAL)
		}
	}()

	return nil
}

func (s *AnomalyScanner) checkDatabaseAnomaly(ctx context.Context, instance *api.Instance, database *api.Database) {
	driver, err := GetDatabaseDriver(instance, "", s.l)

	// Check connection
	if err != nil {
		anomalyPayload := api.AnomalyDatabaseConnectionPayload{
			Detail: err.Error(),
		}
		payload, err := json.Marshal(anomalyPayload)
		if err != nil {
			s.l.Error("Failed to marshal anomaly payload",
				zap.String("instance", instance.Name),
				zap.String("database", database.Name),
				zap.String("type", string(api.AnomalyDatabaseConnection)),
				zap.Error(err))
		} else {
			_, err = s.server.AnomalyService.UpsertActiveAnomaly(ctx, &api.AnomalyUpsert{
				CreatorId:  api.SYSTEM_BOT_ID,
				InstanceId: instance.ID,
				DatabaseId: database.ID,
				Type:       api.AnomalyDatabaseConnection,
				Payload:    string(payload),
			})
			if err != nil {
				s.l.Error("Failed to create anomaly",
					zap.String("instance", instance.Name),
					zap.String("database", database.Name),
					zap.String("type", string(api.AnomalyDatabaseConnection)),
					zap.Error(err))
			}
		}
		return
	} else {
		defer driver.Close(ctx)
		err := s.server.AnomalyService.ArchiveAnomaly(ctx, &api.AnomalyArchive{
			DatabaseId: database.ID,
			Type:       api.AnomalyDatabaseConnection,
		})
		if err != nil && common.ErrorCode(err) != common.NotFound {
			s.l.Error("Failed to close anomaly",
				zap.String("instance", instance.Name),
				zap.String("database", database.Name),
				zap.String("type", string(api.AnomalyDatabaseConnection)),
				zap.Error(err))
		}
	}

	// Check schema drift
	{
		var schemaBuf bytes.Buffer
		if err := driver.Dump(ctx, database.Name, &schemaBuf, true /*schemaOnly*/); err != nil {
			if common.ErrorCode(err) == common.NotFound {
				s.l.Debug("Failed to check anomaly",
					zap.String("instance", instance.Name),
					zap.String("database", database.Name),
					zap.String("type", string(api.AnomalyDatabaseSchemaDrift)),
					zap.Error(err))
			} else {
				s.l.Error("Failed to check anomaly",
					zap.String("instance", instance.Name),
					zap.String("database", database.Name),
					zap.String("type", string(api.AnomalyDatabaseSchemaDrift)),
					zap.Error(err))
			}
			goto SchemaDriftEnd
		}
		limit := 1
		list, err := driver.FindMigrationHistoryList(ctx, &db.MigrationHistoryFind{
			Database: &database.Name,
			Limit:    &limit,
		})
		if err != nil {
			s.l.Error("Failed to check anomaly",
				zap.String("instance", instance.Name),
				zap.String("database", database.Name),
				zap.String("type", string(api.AnomalyDatabaseSchemaDrift)),
				zap.Error(err))
			goto SchemaDriftEnd
		}
		if len(list) > 0 {
			if list[0].Schema != schemaBuf.String() {
				anomalyPayload := api.AnomalyDatabaseSchemaDriftPayload{
					Version: list[0].Version,
					Expect:  list[0].Schema,
					Actual:  schemaBuf.String(),
				}
				payload, err := json.Marshal(anomalyPayload)
				if err != nil {
					s.l.Error("Failed to marshal anomaly payload",
						zap.String("instance", instance.Name),
						zap.String("database", database.Name),
						zap.String("type", string(api.AnomalyDatabaseSchemaDrift)),
						zap.Error(err))
				} else {
					_, err = s.server.AnomalyService.UpsertActiveAnomaly(ctx, &api.AnomalyUpsert{
						CreatorId:  api.SYSTEM_BOT_ID,
						InstanceId: instance.ID,
						DatabaseId: database.ID,
						Type:       api.AnomalyDatabaseSchemaDrift,
						Payload:    string(payload),
					})
					if err != nil {
						s.l.Error("Failed to create anomaly",
							zap.String("instance", instance.Name),
							zap.String("database", database.Name),
							zap.String("type", string(api.AnomalyDatabaseSchemaDrift)),
							zap.Error(err))
					}
				}
			} else {
				err := s.server.AnomalyService.ArchiveAnomaly(ctx, &api.AnomalyArchive{
					DatabaseId: database.ID,
					Type:       api.AnomalyDatabaseConnection,
				})
				if err != nil && common.ErrorCode(err) != common.NotFound {
					s.l.Error("Failed to close anomaly",
						zap.String("instance", instance.Name),
						zap.String("database", database.Name),
						zap.String("type", string(api.AnomalyDatabaseSchemaDrift)),
						zap.Error(err))
				}
			}
		}
	}
SchemaDriftEnd:
}

func (s *AnomalyScanner) checkBackupAnomaly(ctx context.Context, instance *api.Instance, database *api.Database, policyMap map[int]*api.BackupPlanPolicy) {
	schedule := api.BackupPlanPolicyScheduleUnset
	backupSettingFind := &api.BackupSettingFind{
		DatabaseId: &database.ID,
	}
	backupSetting, err := s.server.BackupService.FindBackupSetting(ctx, backupSettingFind)
	if err != nil {
		if common.ErrorCode(err) != common.NotFound {
			s.l.Error("Failed to retrieve backup setting",
				zap.String("instance", instance.Name),
				zap.String("database", database.Name),
				zap.Error(err))
			return
		}
	} else {
		if backupSetting.Enabled && backupSetting.Hour != -1 {
			if backupSetting.DayOfWeek == -1 {
				schedule = api.BackupPlanPolicyScheduleDaily
			} else {
				schedule = api.BackupPlanPolicyScheduleWeekly
			}
		}
	}

	// Check backup policy violation
	{
		var backupPolicyAnomalyPayload *api.AnomalyDatabaseBackupPolicyViolationPayload
		if policyMap[instance.EnvironmentId].Schedule != api.BackupPlanPolicyScheduleUnset {
			if policyMap[instance.EnvironmentId].Schedule == api.BackupPlanPolicyScheduleDaily &&
				schedule != api.BackupPlanPolicyScheduleDaily {
				backupPolicyAnomalyPayload = &api.AnomalyDatabaseBackupPolicyViolationPayload{
					EnvironmentId:          instance.EnvironmentId,
					ExpectedBackupSchedule: policyMap[instance.EnvironmentId].Schedule,
					ActualBackupSchedule:   schedule,
				}
			} else if policyMap[instance.EnvironmentId].Schedule == api.BackupPlanPolicyScheduleWeekly &&
				schedule == api.BackupPlanPolicyScheduleUnset {
				backupPolicyAnomalyPayload = &api.AnomalyDatabaseBackupPolicyViolationPayload{
					EnvironmentId:          instance.EnvironmentId,
					ExpectedBackupSchedule: policyMap[instance.EnvironmentId].Schedule,
					ActualBackupSchedule:   schedule,
				}
			}
		}

		if backupPolicyAnomalyPayload != nil {
			payload, err := json.Marshal(*backupPolicyAnomalyPayload)
			if err != nil {
				s.l.Error("Failed to marshal anomaly payload",
					zap.String("instance", instance.Name),
					zap.String("database", database.Name),
					zap.String("type", string(api.AnomalyDatabaseBackupPolicyViolation)),
					zap.Error(err))
			} else {
				_, err = s.server.AnomalyService.UpsertActiveAnomaly(ctx, &api.AnomalyUpsert{
					CreatorId:  api.SYSTEM_BOT_ID,
					InstanceId: instance.ID,
					DatabaseId: database.ID,
					Type:       api.AnomalyDatabaseBackupPolicyViolation,
					Payload:    string(payload),
				})
				if err != nil {
					s.l.Error("Failed to create anomaly",
						zap.String("instance", instance.Name),
						zap.String("database", database.Name),
						zap.String("type", string(api.AnomalyDatabaseBackupPolicyViolation)),
						zap.Error(err))
				}
			}
		} else {
			err := s.server.AnomalyService.ArchiveAnomaly(ctx, &api.AnomalyArchive{
				DatabaseId: database.ID,
				Type:       api.AnomalyDatabaseBackupPolicyViolation,
			})
			if err != nil && common.ErrorCode(err) != common.NotFound {
				s.l.Error("Failed to close anomaly",
					zap.String("instance", instance.Name),
					zap.String("database", database.Name),
					zap.String("type", string(api.AnomalyDatabaseBackupPolicyViolation)),
					zap.Error(err))
			}
		}
	}

	// Check backup missing
	{
		var backupMissingAnomalyPayload *api.AnomalyDatabaseBackupMissingPayload
		// The anomaly fires if backup is enabled, however no succesful backup has been taken during the period.
		if backupSetting != nil && backupSetting.Enabled {
			expectedSchedule := api.BackupPlanPolicyScheduleWeekly
			backupMaxAge := time.Duration(7*24) * time.Hour
			if backupSetting.DayOfWeek == -1 {
				expectedSchedule = api.BackupPlanPolicyScheduleDaily
				backupMaxAge = time.Duration(24) * time.Hour
			}

			// Ignore if backup setting has been changed after the max age.
			if backupSetting.UpdatedTs < time.Now().Add(-backupMaxAge).Unix() {
				status := api.BackupStatusDone
				backupFind := &api.BackupFind{
					DatabaseId: &database.ID,
					Status:     &status,
				}
				backupList, err := s.server.BackupService.FindBackupList(ctx, backupFind)
				if err != nil {
					s.l.Error("Failed to retrieve backup list",
						zap.String("instance", instance.Name),
						zap.String("database", database.Name),
						zap.Error(err))
				}

				hasValidBackup := false
				if len(backupList) > 0 {
					if backupList[0].UpdatedTs >= time.Now().Add(-backupMaxAge).Unix() {
						hasValidBackup = true
					}
				}

				if !hasValidBackup {
					backupMissingAnomalyPayload = &api.AnomalyDatabaseBackupMissingPayload{
						ExpectedBackupSchedule: expectedSchedule,
					}
					if len(backupList) > 0 {
						backupMissingAnomalyPayload.LastBackupTs = backupList[0].UpdatedTs
					}
				}
			}
		}

		if backupMissingAnomalyPayload != nil {
			payload, err := json.Marshal(*backupMissingAnomalyPayload)
			if err != nil {
				s.l.Error("Failed to marshal anomaly payload",
					zap.String("instance", instance.Name),
					zap.String("database", database.Name),
					zap.String("type", string(api.AnomalyDatabaseBackupMissing)),
					zap.Error(err))
			} else {
				_, err = s.server.AnomalyService.UpsertActiveAnomaly(ctx, &api.AnomalyUpsert{
					CreatorId:  api.SYSTEM_BOT_ID,
					InstanceId: instance.ID,
					DatabaseId: database.ID,
					Type:       api.AnomalyDatabaseBackupMissing,
					Payload:    string(payload),
				})
				if err != nil {
					s.l.Error("Failed to create anomaly",
						zap.String("instance", instance.Name),
						zap.String("database", database.Name),
						zap.String("type", string(api.AnomalyDatabaseBackupMissing)),
						zap.Error(err))
				}
			}
		} else {
			err := s.server.AnomalyService.ArchiveAnomaly(ctx, &api.AnomalyArchive{
				DatabaseId: database.ID,
				Type:       api.AnomalyDatabaseBackupMissing,
			})
			if err != nil && common.ErrorCode(err) != common.NotFound {
				s.l.Error("Failed to close anomaly",
					zap.String("instance", instance.Name),
					zap.String("database", database.Name),
					zap.String("type", string(api.AnomalyDatabaseBackupMissing)),
					zap.Error(err))
			}
		}
	}
}
