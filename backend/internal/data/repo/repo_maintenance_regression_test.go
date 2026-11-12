package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	goent "entgo.io/ent"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/sysadminsmedia/homebox/backend/internal/data/ent"
	"github.com/sysadminsmedia/homebox/backend/internal/data/ent/maintenanceentry"
	"github.com/sysadminsmedia/homebox/backend/internal/data/types"
)

func TestMaintenanceUpdateClearsValues(t *testing.T) {
	ctx := context.Background()
	item := useEntities(t, 1)[0]
	entry, err := tRepos.MaintEntry.Create(ctx, tGroup.ID, item.ID, MaintenanceEntryCreate{
		Name: "Filter", Description: "old", Cost: 40,
		ScheduledDate: types.DateFromString("2026-01-01"), CompletedDate: types.DateFromString("2026-01-02"),
	})
	require.NoError(t, err)
	updated, err := tRepos.MaintEntry.Update(ctx, tGroup.ID, entry.ID, MaintenanceEntryUpdate{
		Name: entry.Name, ScheduledDate: entry.ScheduledDate,
	})
	require.NoError(t, err)
	assert.Empty(t, updated.Description)
	assert.Zero(t, updated.Cost)
	assert.True(t, updated.CompletedDate.Time().IsZero())
}

func TestMaintenanceUpdateOmittedPlanPreservesRecurrence(t *testing.T) {
	ctx := context.Background()
	item := useEntities(t, 1)[0]
	plan := useMaintenancePlan(t, item.ID, time.Now().UTC())
	entry, err := tRepos.MaintEntry.db.MaintenanceEntry.Query().Where(maintenanceentry.PlanIDEQ(plan.ID)).Only(ctx)
	require.NoError(t, err)
	updated, err := tRepos.MaintEntry.Update(ctx, tGroup.ID, entry.ID, MaintenanceEntryUpdate{
		Name: entry.Name, ScheduledDate: types.DateFromDBTime(entry.ScheduledDate), CompletedDate: types.DateFromString("2026-11-12"),
	})
	require.NoError(t, err)
	assert.Equal(t, plan.ID, updated.PlanID)
	count, err := tRepos.MaintEntry.db.MaintenanceEntry.Query().Where(maintenanceentry.PlanIDEQ(plan.ID)).Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, count)
	// An explicit nil UUID detaches; omission does not.
	var detach MaintenanceEntryUpdate
	require.NoError(t, json.Unmarshal([]byte(`{"name":"Filter","scheduledDate":"2026-11-12","planID":"00000000-0000-0000-0000-000000000000"}`), &detach))
	updated, err = tRepos.MaintEntry.Update(ctx, tGroup.ID, entry.ID, detach)
	require.NoError(t, err)
	assert.Equal(t, uuid.Nil, updated.PlanID)
}

func TestMaintenanceTodayIsNotOverdue(t *testing.T) {
	ctx := context.Background()
	item := useEntities(t, 1)[0]
	today := types.DateFromTime(time.Now().UTC())
	entry, err := tRepos.MaintEntry.Create(ctx, tGroup.ID, item.ID, MaintenanceEntryCreate{Name: "Today", ScheduledDate: today})
	require.NoError(t, err)
	assert.False(t, entry.IsOverdue)
	// An undated entry must also stay out of the overdue filter.
	_, err = tRepos.MaintEntry.db.MaintenanceEntry.Create().SetEntityID(item.ID).SetName("Undated").SetDate(time.Time{}).SetScheduledDate(time.Time{}).Save(ctx)
	require.NoError(t, err)
	entries, err := tRepos.MaintEntry.GetMaintenanceByItemID(ctx, tGroup.ID, item.ID, MaintenanceFilters{Status: MaintenanceFilterStatusOverdue})
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestMaintenancePlanRejectsUnrepresentableIntervals(t *testing.T) {
	for _, unit := range []MaintenancePlanIntervalUnit{MaintenancePlanIntervalUnitHour, MaintenancePlanIntervalUnitDay, MaintenancePlanIntervalUnitWeek, MaintenancePlanIntervalUnitMonth, MaintenancePlanIntervalUnitYear} {
		t.Run(string(unit), func(t *testing.T) {
			require.Error(t, (MaintenancePlanCreate{IntervalValue: int(^uint(0) >> 1), IntervalUnit: unit}).Validate())
			require.Error(t, (MaintenancePlanUpdate{IntervalValue: int(^uint(0) >> 1), IntervalUnit: unit}).Validate())
		})
	}
	require.Error(t, (MaintenancePlanCreate{IntervalValue: 1, IntervalUnit: MaintenancePlanIntervalUnitHour}).Validate())
	require.Error(t, (MaintenancePlanCreate{IntervalValue: 1, IntervalUnit: MaintenancePlanIntervalUnitYear, StartDate: types.DateFromString("9999-01-01")}).Validate())
}

func TestMaintenancePlanLinksCompletedHistory(t *testing.T) {
	ctx := context.Background()
	item := useEntities(t, 1)[0]
	entry, err := tRepos.MaintEntry.Create(ctx, tGroup.ID, item.ID, MaintenanceEntryCreate{
		Name: "Changed filters", Cost: 45, ScheduledDate: types.DateFromString("2026-01-01"), CompletedDate: types.DateFromString("2026-01-05"),
	})
	require.NoError(t, err)
	plan, err := tRepos.MaintEntry.CreatePlan(ctx, tGroup.ID, item.ID, MaintenancePlanCreate{
		Name: "Filters", IntervalValue: 6, IntervalUnit: MaintenancePlanIntervalUnitMonth, Active: true,
		StartDate: types.DateFromString("2026-02-01"), LinkExistingEntryID: &entry.ID,
	})
	require.NoError(t, err)
	assert.Equal(t, "2026-07-05", plan.NextDueAt.UTC().Format(time.DateOnly))
	assert.Equal(t, "2026-01-05", plan.LastCompletedAt.UTC().Format(time.DateOnly))
	history, err := tRepos.MaintEntry.db.MaintenanceEntry.Get(ctx, entry.ID)
	require.NoError(t, err)
	assert.Equal(t, "2026-01-01", history.ScheduledDate.UTC().Format(time.DateOnly))
	assert.InDelta(t, 45.0, history.Cost, 0.001)
	entries, err := tRepos.MaintEntry.db.MaintenanceEntry.Query().Where(maintenanceentry.PlanIDEQ(plan.ID)).All(ctx)
	require.NoError(t, err)
	assert.Len(t, entries, 2)
}

func TestMaintenancePlanCreatesCompletedFirstEntry(t *testing.T) {
	ctx := context.Background()
	item := useEntities(t, 1)[0]
	var input MaintenancePlanCreate
	require.NoError(t, json.Unmarshal([]byte(`{"name":"Filter","description":"Two cartridges","intervalValue":6,"intervalUnit":"month","active":true,"startDate":"2026-08-31","completedDate":"2026-08-31","cost":"42.50"}`), &input))
	plan, err := tRepos.MaintEntry.CreatePlan(ctx, tGroup.ID, item.ID, input)
	require.NoError(t, err)
	assert.Equal(t, "2027-02-28", plan.NextDueAt.UTC().Format(time.DateOnly))
	history, err := tRepos.MaintEntry.db.MaintenanceEntry.Query().Where(maintenanceentry.PlanIDEQ(plan.ID), maintenanceentry.DateEQ(types.DateFromString("2026-08-31").Time())).Only(ctx)
	require.NoError(t, err)
	assert.InDelta(t, 42.5, history.Cost, 0.001)
	assert.Equal(t, "Two cartridges", history.Description)
}

func TestMaintenancePlanReschedulesOpenEntry(t *testing.T) {
	ctx := context.Background()
	item := useEntities(t, 1)[0]
	plan := useMaintenancePlan(t, item.ID, types.DateFromString("2026-07-01").Time())
	next := types.DateFromString("2026-08-01")
	_, err := tRepos.MaintEntry.UpdatePlan(ctx, tGroup.ID, plan.ID, MaintenancePlanUpdate{
		Name: plan.Name, IntervalValue: plan.IntervalValue, IntervalUnit: plan.IntervalUnit, Active: true, NextDueAt: &next,
	})
	require.NoError(t, err)
	entry, err := tRepos.MaintEntry.db.MaintenanceEntry.Query().Where(maintenanceentry.PlanIDEQ(plan.ID)).Only(ctx)
	require.NoError(t, err)
	assert.True(t, next.Time().Equal(entry.ScheduledDate))
}

func TestMaintenanceWaterFilterThreePlans(t *testing.T) {
	item := useEntities(t, 1)[0]
	testWaterFilterPlans(t, tRepos.MaintEntry, tGroup.ID, item.ID)
}

func testWaterFilterPlans(t *testing.T, repository *MaintenanceEntryRepository, gid, itemID uuid.UUID) {
	const twoCartridges = "2 cartridges"
	t.Helper()
	ctx := context.Background()
	for _, spec := range []struct {
		name, start string
		years       int
	}{
		{twoCartridges, "2026-07-01", 1}, {"3 cartridges", "2027-01-01", 2}, {"4 cartridges", "2028-01-01", 2},
	} {
		_, err := repository.CreatePlan(ctx, gid, itemID, MaintenancePlanCreate{
			Name: spec.name, StartDate: types.DateFromString(spec.start), IntervalValue: spec.years, IntervalUnit: MaintenancePlanIntervalUnitYear, Active: true,
		})
		require.NoError(t, err)
	}
	for _, visit := range []struct{ date, name string }{
		{"2026-07-01", twoCartridges}, {"2027-01-01", "3 cartridges"}, {"2027-07-01", twoCartridges}, {"2028-01-01", "4 cartridges"},
		{"2028-07-01", twoCartridges}, {"2029-01-01", "3 cartridges"}, {"2029-07-01", twoCartridges}, {"2030-01-01", "4 cartridges"},
	} {
		entry, err := repository.db.MaintenanceEntry.Query().Where(maintenanceentry.EntityID(itemID), maintenanceentry.ScheduledDateEQ(types.DateFromString(visit.date).Time()), maintenanceentry.DateEQ(time.Time{})).Only(ctx)
		require.NoError(t, err, visit.date)
		assert.Equal(t, visit.name, entry.Name)
		input := MaintenanceEntryUpdate{Name: entry.Name, ScheduledDate: types.DateFromDBTime(entry.ScheduledDate), CompletedDate: types.DateFromString(visit.date)}
		_, err = repository.Update(ctx, gid, entry.ID, input)
		require.NoError(t, err)
		// Retrying completion must not create another next occurrence.
		_, err = repository.Update(ctx, gid, entry.ID, input)
		require.NoError(t, err)
		count, err := repository.db.MaintenanceEntry.Query().Where(maintenanceentry.EntityID(itemID), maintenanceentry.DateEQ(time.Time{})).Count(ctx)
		require.NoError(t, err)
		require.Equal(t, 3, count)
	}
}

func TestMaintenanceRecurringHistoryCannotBeReopened(t *testing.T) {
	ctx := context.Background()
	item := useEntities(t, 1)[0]
	plan := useMaintenancePlan(t, item.ID, types.DateFromString("2026-01-01").Time())
	entry, err := tRepos.MaintEntry.db.MaintenanceEntry.Query().Where(maintenanceentry.PlanIDEQ(plan.ID)).Only(ctx)
	require.NoError(t, err)
	input := MaintenanceEntryUpdate{Name: entry.Name, ScheduledDate: types.DateFromDBTime(entry.ScheduledDate), CompletedDate: types.DateFromString("2026-01-02")}
	_, err = tRepos.MaintEntry.Update(ctx, tGroup.ID, entry.ID, input)
	require.NoError(t, err)
	input.CompletedDate = types.Date{}
	_, err = tRepos.MaintEntry.Update(ctx, tGroup.ID, entry.ID, input)
	require.Error(t, err)
	stored, err := tRepos.MaintEntry.db.MaintenanceEntry.Get(ctx, entry.ID)
	require.NoError(t, err)
	assert.Equal(t, "2026-01-02", stored.Date.UTC().Format(time.DateOnly))
}

func TestMaintenanceOverflowRollsBackCompletion(t *testing.T) {
	ctx := context.Background()
	item := useEntities(t, 1)[0]
	plan := useMaintenancePlan(t, item.ID, types.DateFromString("2026-01-01").Time())
	entry, err := tRepos.MaintEntry.db.MaintenanceEntry.Query().Where(maintenanceentry.PlanIDEQ(plan.ID)).Only(ctx)
	require.NoError(t, err)
	_, err = tRepos.MaintEntry.Update(ctx, tGroup.ID, entry.ID, MaintenanceEntryUpdate{Name: entry.Name, ScheduledDate: types.DateFromDBTime(entry.ScheduledDate), CompletedDate: types.DateFromString("9999-12-31")})
	require.Error(t, err)
	stored, err := tRepos.MaintEntry.db.MaintenanceEntry.Get(ctx, entry.ID)
	require.NoError(t, err)
	assert.True(t, stored.Date.IsZero())
	count, err := tRepos.MaintEntry.db.MaintenanceEntry.Query().Where(maintenanceentry.PlanIDEQ(plan.ID)).Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

func TestMaintenanceCannotStealLinkedEntry(t *testing.T) {
	ctx := context.Background()
	item := useEntities(t, 1)[0]
	plan := useMaintenancePlan(t, item.ID, types.DateFromString("2026-01-01").Time())
	entry, err := tRepos.MaintEntry.db.MaintenanceEntry.Query().Where(maintenanceentry.PlanIDEQ(plan.ID)).Only(ctx)
	require.NoError(t, err)
	_, err = tRepos.MaintEntry.CreatePlan(ctx, tGroup.ID, item.ID, MaintenancePlanCreate{Name: "Other", IntervalValue: 1, IntervalUnit: MaintenancePlanIntervalUnitYear, Active: true, LinkExistingEntryID: &entry.ID})
	require.Error(t, err)
	plans, err := tRepos.MaintEntry.ListPlansByItemID(ctx, tGroup.ID, item.ID)
	require.NoError(t, err)
	assert.Len(t, plans, 1)
}

func TestMaintenancePausedRemindersAndExplicitResume(t *testing.T) {
	ctx := context.Background()
	item := useEntities(t, 1)[0]
	today := types.DateFromTime(time.Now())
	plan, err := tRepos.MaintEntry.CreatePlan(ctx, tGroup.ID, item.ID, MaintenancePlanCreate{Name: "Paused", IntervalValue: 6, IntervalUnit: MaintenancePlanIntervalUnitMonth, StartDate: today, Active: false})
	require.NoError(t, err)
	scheduled, err := tRepos.MaintEntry.GetScheduled(ctx, tGroup.ID, today)
	require.NoError(t, err)
	for _, entry := range scheduled {
		assert.NotEqual(t, plan.ID, entry.PlanID)
	}
	entry, err := tRepos.MaintEntry.db.MaintenanceEntry.Query().Where(maintenanceentry.PlanIDEQ(plan.ID)).Only(ctx)
	require.NoError(t, err)
	_, err = tRepos.MaintEntry.Update(ctx, tGroup.ID, entry.ID, MaintenanceEntryUpdate{Name: entry.Name, ScheduledDate: today, CompletedDate: today})
	require.NoError(t, err)
	due := types.DateFromString("2030-05-01")
	updated, err := tRepos.MaintEntry.UpdatePlan(ctx, tGroup.ID, plan.ID, MaintenancePlanUpdate{Name: plan.Name, IntervalValue: 6, IntervalUnit: MaintenancePlanIntervalUnitMonth, Active: true, NextDueAt: &due})
	require.NoError(t, err)
	assert.True(t, due.Time().Equal(updated.NextDueAt))
	pending, err := tRepos.MaintEntry.db.MaintenanceEntry.Query().Where(maintenanceentry.PlanIDEQ(plan.ID), maintenanceentry.DateEQ(time.Time{})).Only(ctx)
	require.NoError(t, err)
	assert.True(t, due.Time().Equal(pending.ScheduledDate))
}

func TestMaintenancePlanDatesAreSerializedInUTC(t *testing.T) {
	utc := types.DateFromString("2026-08-31").Time()
	driverDate := utc.In(time.FixedZone("Chicago", -6*3600))
	mapped := mapMaintenancePlan(&ent.MaintenancePlan{NextDueAt: &driverDate, LastCompletedAt: &driverDate})
	assert.Equal(t, "2026-08-31", mapped.NextDueAt.Format(time.DateOnly))
	assert.Equal(t, "2026-08-31", mapped.LastCompletedAt.Format(time.DateOnly))
}

func TestMaintenanceLinkedCreationCannotBypassPlan(t *testing.T) {
	ctx := context.Background()
	item := useEntities(t, 1)[0]
	plan := useMaintenancePlan(t, item.ID, time.Now())
	_, err := tRepos.MaintEntry.Create(ctx, tGroup.ID, item.ID, MaintenanceEntryCreate{Name: "Extra", PlanID: plan.ID, ScheduledDate: types.DateFromString("2027-01-01")})
	require.Error(t, err)
	entry, err := tRepos.MaintEntry.Create(ctx, tGroup.ID, item.ID, MaintenanceEntryCreate{Name: "Standalone", ScheduledDate: types.DateFromString("2027-01-01")})
	require.NoError(t, err)
	_, err = tRepos.MaintEntry.Update(ctx, tGroup.ID, entry.ID, MaintenanceEntryUpdate{Name: entry.Name, ScheduledDate: entry.ScheduledDate, PlanID: &plan.ID})
	require.Error(t, err)
}

func TestMaintenancePostgresWaterFilterPlans(t *testing.T) {
	client, gid, itemID := usePostgresMaintenance(t)
	testWaterFilterPlans(t, &MaintenanceEntryRepository{db: client}, gid, itemID)
}

func usePostgresMaintenance(t *testing.T) (*ent.Client, uuid.UUID, uuid.UUID) {
	t.Helper()
	dsn := os.Getenv("TEST_MAINTENANCE_POSTGRES_URL")
	if dsn == "" {
		t.Skip("set TEST_MAINTENANCE_POSTGRES_URL to test PostgreSQL lifecycle")
	}
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	client := ent.NewClient(ent.Driver(entsql.OpenDB("postgres", db)))
	t.Cleanup(func() { _ = client.Close() })
	ctx := context.Background()
	require.NoError(t, client.Schema.Create(ctx))
	group, err := client.Group.Create().SetName("Maintenance regression").Save(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Group.DeleteOneID(group.ID).Exec(ctx) })
	entityType, err := client.EntityType.Create().SetGroupID(group.ID).SetName("Item").Save(ctx)
	require.NoError(t, err)
	item, err := client.Entity.Create().SetGroupID(group.ID).SetEntityTypeID(entityType.ID).SetName("Water filter").Save(ctx)
	require.NoError(t, err)
	return client, group.ID, item.ID
}

func TestMaintenancePostgresCompletionRejectsConcurrentLink(t *testing.T) {
	client, gid, itemID := usePostgresMaintenance(t)
	repository := &MaintenanceEntryRepository{db: client}
	ctx := context.Background()
	today := types.DateFromTime(time.Now())
	entry, err := repository.Create(ctx, gid, itemID, MaintenanceEntryCreate{Name: "Standalone", ScheduledDate: today})
	require.NoError(t, err)
	reached, release := make(chan struct{}), make(chan struct{})
	var once, releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	client.MaintenanceEntry.Use(func(next goent.Mutator) goent.Mutator {
		return goent.MutateFunc(func(ctx context.Context, m goent.Mutation) (goent.Value, error) {
			mutation := m.(*ent.MaintenanceEntryMutation)
			if date, ok := mutation.Date(); ok && !date.IsZero() {
				once.Do(func() { close(reached); <-release })
			}
			return next.Mutate(ctx, m)
		})
	})
	completed := make(chan error, 1)
	go func() {
		_, err := repository.Update(ctx, gid, entry.ID, MaintenanceEntryUpdate{Name: entry.Name, ScheduledDate: today, CompletedDate: today})
		completed <- err
	}()
	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		t.Fatal("completion did not reach entry update")
	}
	plan, err := repository.CreatePlan(ctx, gid, itemID, MaintenancePlanCreate{Name: entry.Name, IntervalValue: 6, IntervalUnit: MaintenancePlanIntervalUnitMonth, StartDate: today, Active: true, LinkExistingEntryID: &entry.ID})
	require.NoError(t, err)
	releaseOnce.Do(func() { close(release) })
	require.Error(t, <-completed, "a stale completion must not bypass plan advancement")
	pending, err := client.MaintenanceEntry.Query().Where(maintenanceentry.PlanIDEQ(plan.ID), maintenanceentry.DateEQ(time.Time{})).Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, pending)
}

func TestMaintenancePostgresCompletionSerializesResume(t *testing.T) {
	client, gid, itemID := usePostgresMaintenance(t)
	repository := &MaintenanceEntryRepository{db: client}
	ctx := context.Background()
	today := types.DateFromTime(time.Now())
	plan, err := repository.CreatePlan(ctx, gid, itemID, MaintenancePlanCreate{Name: "Paused", IntervalValue: 6, IntervalUnit: MaintenancePlanIntervalUnitMonth, StartDate: today, Active: false})
	require.NoError(t, err)
	entry, err := client.MaintenanceEntry.Query().Where(maintenanceentry.PlanIDEQ(plan.ID)).Only(ctx)
	require.NoError(t, err)
	reached := make(chan struct{})
	release := make(chan struct{})
	var once, releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	client.MaintenancePlan.Use(func(next goent.Mutator) goent.Mutator {
		return goent.MutateFunc(func(ctx context.Context, m goent.Mutation) (goent.Value, error) {
			mutation := m.(*ent.MaintenancePlanMutation)
			if _, ok := mutation.LastCompletedAt(); ok {
				once.Do(func() { close(reached); <-release })
			}
			return next.Mutate(ctx, m)
		})
	})
	completed := make(chan error, 1)
	go func() {
		_, err := repository.Update(ctx, gid, entry.ID, MaintenanceEntryUpdate{Name: entry.Name, ScheduledDate: today, CompletedDate: today})
		completed <- err
	}()
	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		t.Fatal("completion did not reach plan update")
	}
	// Another connection must not be able to change Active/interval after the
	// completing transaction has chosen how to advance this plan.
	var id uuid.UUID
	err = client.Sql().QueryRowContext(ctx, "SELECT id FROM maintenance_plans WHERE id = $1 FOR UPDATE NOWAIT", plan.ID).Scan(&id)
	require.Error(t, err, "completion must lock the plan before reading its active state")
	if err != nil {
		assert.Contains(t, err.Error(), "could not obtain lock")
	}
	resumed := make(chan error, 1)
	go func() {
		_, err := repository.UpdatePlan(ctx, gid, plan.ID, MaintenancePlanUpdate{Name: plan.Name, IntervalValue: 6, IntervalUnit: MaintenancePlanIntervalUnitMonth, Active: true})
		resumed <- err
	}()
	releaseOnce.Do(func() { close(release) })
	require.NoError(t, <-completed)
	require.NoError(t, <-resumed)
	pending, err := client.MaintenanceEntry.Query().Where(maintenanceentry.PlanIDEQ(plan.ID), maintenanceentry.DateEQ(time.Time{})).Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, pending)
}
