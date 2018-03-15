package sqlstore

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"github.com/xformation/sdp/pkg/bus"
	m "github.com/xformation/sdp/pkg/models"
)

func init() {
	bus.AddHandler("sql", SaveAlerts)
	bus.AddHandler("sql", HandleAlertsQuery)
	bus.AddHandler("sql", GetAlertById)
	bus.AddHandler("sql", GetAllAlertQueryHandler)
	bus.AddHandler("sql", SetAlertState)
	bus.AddHandler("sql", GetAlertStatesForDashboard)
	bus.AddHandler("sql", PauseAlert)
	bus.AddHandler("sql", PauseAllAlerts)
}

func GetAlertById(query *m.GetAlertByIdQuery) error {
	alert := m.Alert{}
	has, err := x.ID(query.Id).Get(&alert)
	if !has {
		return fmt.Errorf("could not find alert")
	}
	if err != nil {
		return err
	}

	query.Result = &alert
	return nil
}

func GetAllAlertQueryHandler(query *m.GetAllAlertsQuery) error {
	var alerts []*m.Alert
	err := x.Sql("select * from alert").Find(&alerts)
	if err != nil {
		return err
	}

	query.Result = alerts
	return nil
}

func deleteAlertByIdInternal(alertId int64, reason string, sess *DBSession) error {
	sqlog.Debug("Deleting alert", "id", alertId, "reason", reason)

	if _, err := sess.Exec("DELETE FROM alert WHERE id = ?", alertId); err != nil {
		return err
	}

	if _, err := sess.Exec("DELETE FROM annotation WHERE alert_id = ?", alertId); err != nil {
		return err
	}

	return nil
}

func HandleAlertsQuery(query *m.GetAlertsQuery) error {
	builder := SqlBuilder{}

	builder.Write(`SELECT
		alert.id,
		alert.dashboard_id,
		alert.panel_id,
		alert.name,
		alert.state,
		alert.new_state_date,
		alert.eval_date,
		alert.execution_error,
		dashboard.uid as dashboard_uid,
		dashboard.slug as dashboard_slug
		FROM alert
		INNER JOIN dashboard on dashboard.id = alert.dashboard_id `)

	builder.Write(`WHERE alert.org_id = ?`, query.OrgId)

	if query.DashboardId != 0 {
		builder.Write(` AND alert.dashboard_id = ?`, query.DashboardId)
	}

	if query.PanelId != 0 {
		builder.Write(` AND alert.panel_id = ?`, query.PanelId)
	}

	if len(query.State) > 0 && query.State[0] != "all" {
		builder.Write(` AND (`)
		for i, v := range query.State {
			if i > 0 {
				builder.Write(" OR ")
			}
			if strings.HasPrefix(v, "not_") {
				builder.Write("state <> ? ")
				v = strings.TrimPrefix(v, "not_")
			} else {
				builder.Write("state = ? ")
			}
			builder.AddParams(v)
		}
		builder.Write(")")
	}

	if query.User.OrgRole != m.ROLE_ADMIN {
		builder.writeDashboardPermissionFilter(query.User, m.PERMISSION_EDIT)
	}

	builder.Write(" ORDER BY name ASC")

	if query.Limit != 0 {
		builder.Write(" LIMIT ?", query.Limit)
	}

	alerts := make([]*m.AlertListItemDTO, 0)
	if err := x.SQL(builder.GetSqlString(), builder.params...).Find(&alerts); err != nil {
		return err
	}

	for i := range alerts {
		if alerts[i].ExecutionError == " " {
			alerts[i].ExecutionError = ""
		}
	}

	query.Result = alerts
	return nil
}

func DeleteAlertDefinition(dashboardId int64, sess *DBSession) error {
	alerts := make([]*m.Alert, 0)
	sess.Where("dashboard_id = ?", dashboardId).Find(&alerts)

	for _, alert := range alerts {
		deleteAlertByIdInternal(alert.Id, "Dashboard deleted", sess)
	}

	return nil
}

func SaveAlerts(cmd *m.SaveAlertsCommand) error {
	return inTransaction(func(sess *DBSession) error {
		existingAlerts, err := GetAlertsByDashboardId2(cmd.DashboardId, sess)
		if err != nil {
			return err
		}

		if err := upsertAlerts(existingAlerts, cmd, sess); err != nil {
			return err
		}

		if err := deleteMissingAlerts(existingAlerts, cmd, sess); err != nil {
			return err
		}

		return nil
	})
}

func upsertAlerts(existingAlerts []*m.Alert, cmd *m.SaveAlertsCommand, sess *DBSession) error {
	for _, alert := range cmd.Alerts {
		update := false
		var alertToUpdate *m.Alert

		for _, k := range existingAlerts {
			if alert.PanelId == k.PanelId {
				update = true
				alert.Id = k.Id
				alertToUpdate = k
				break
			}
		}

		if update {
			if alertToUpdate.ContainsUpdates(alert) {
				alert.Updated = time.Now()
				alert.State = alertToUpdate.State
				sess.MustCols("message")
				_, err := sess.Id(alert.Id).Update(alert)
				if err != nil {
					return err
				}

				sqlog.Debug("Alert updated", "name", alert.Name, "id", alert.Id)
			}
		} else {
			alert.Updated = time.Now()
			alert.Created = time.Now()
			alert.State = m.AlertStatePending
			alert.NewStateDate = time.Now()

			_, err := sess.Insert(alert)
			if err != nil {
				return err
			}

			sqlog.Debug("Alert inserted", "name", alert.Name, "id", alert.Id)
		}
	}

	return nil
}

func deleteMissingAlerts(alerts []*m.Alert, cmd *m.SaveAlertsCommand, sess *DBSession) error {
	for _, missingAlert := range alerts {
		missing := true

		for _, k := range cmd.Alerts {
			if missingAlert.PanelId == k.PanelId {
				missing = false
				break
			}
		}

		if missing {
			deleteAlertByIdInternal(missingAlert.Id, "Removed from dashboard", sess)
		}
	}

	return nil
}

func GetAlertsByDashboardId2(dashboardId int64, sess *DBSession) ([]*m.Alert, error) {
	alerts := make([]*m.Alert, 0)
	err := sess.Where("dashboard_id = ?", dashboardId).Find(&alerts)

	if err != nil {
		return []*m.Alert{}, err
	}

	return alerts, nil
}

func SetAlertState(cmd *m.SetAlertStateCommand) error {
	return inTransaction(func(sess *DBSession) error {
		alert := m.Alert{}

		if has, err := sess.Id(cmd.AlertId).Get(&alert); err != nil {
			return err
		} else if !has {
			return fmt.Errorf("Could not find alert")
		}

		if alert.State == m.AlertStatePaused {
			return m.ErrCannotChangeStateOnPausedAlert
		}

		if alert.State == cmd.State {
			return m.ErrRequiresNewState
		}

		alert.State = cmd.State
		alert.StateChanges += 1
		alert.NewStateDate = time.Now()
		alert.EvalData = cmd.EvalData

		if cmd.Error == "" {
			alert.ExecutionError = " " //without this space, xorm skips updating this field
		} else {
			alert.ExecutionError = cmd.Error
		}

		sess.ID(alert.Id).Update(&alert)
		return nil
	})
}

func PauseAlert(cmd *m.PauseAlertCommand) error {
	return inTransaction(func(sess *DBSession) error {
		if len(cmd.AlertIds) == 0 {
			return fmt.Errorf("command contains no alertids")
		}

		var buffer bytes.Buffer
		params := make([]interface{}, 0)

		buffer.WriteString(`UPDATE alert SET state = ?`)
		if cmd.Paused {
			params = append(params, string(m.AlertStatePaused))
		} else {
			params = append(params, string(m.AlertStatePending))
		}

		buffer.WriteString(` WHERE id IN (?` + strings.Repeat(",?", len(cmd.AlertIds)-1) + `)`)
		for _, v := range cmd.AlertIds {
			params = append(params, v)
		}

		res, err := sess.Exec(buffer.String(), params...)
		if err != nil {
			return err
		}
		cmd.ResultCount, _ = res.RowsAffected()
		return nil
	})
}

func PauseAllAlerts(cmd *m.PauseAllAlertCommand) error {
	return inTransaction(func(sess *DBSession) error {
		var newState string
		if cmd.Paused {
			newState = string(m.AlertStatePaused)
		} else {
			newState = string(m.AlertStatePending)
		}

		res, err := sess.Exec(`UPDATE alert SET state = ?`, newState)
		if err != nil {
			return err
		}
		cmd.ResultCount, _ = res.RowsAffected()
		return nil
	})
}

func GetAlertStatesForDashboard(query *m.GetAlertStatesForDashboardQuery) error {
	var rawSql = `SELECT
	                id,
	                dashboard_id,
	                panel_id,
	                state,
	                new_state_date
	                FROM alert
	                WHERE org_id = ? AND dashboard_id = ?`

	query.Result = make([]*m.AlertStateInfoDTO, 0)
	err := x.SQL(rawSql, query.OrgId, query.DashboardId).Find(&query.Result)

	return err
}
