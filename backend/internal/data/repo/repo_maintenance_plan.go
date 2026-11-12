package repo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/sysadminsmedia/homebox/backend/internal/data/ent"
	"github.com/sysadminsmedia/homebox/backend/internal/data/ent/entity"
	"github.com/sysadminsmedia/homebox/backend/internal/data/ent/group"
	"github.com/sysadminsmedia/homebox/backend/internal/data/ent/maintenanceentry"
	"github.com/sysadminsmedia/homebox/backend/internal/data/ent/maintenanceplan"
	"github.com/sysadminsmedia/homebox/backend/internal/data/types"
	"github.com/sysadminsmedia/homebox/backend/internal/sys/validate"
)

type MaintenancePlanIntervalUnit string

const (
	MaintenancePlanIntervalUnitHour  MaintenancePlanIntervalUnit = "hour"
	MaintenancePlanIntervalUnitDay   MaintenancePlanIntervalUnit = "day"
	MaintenancePlanIntervalUnitWeek  MaintenancePlanIntervalUnit = "week"
	MaintenancePlanIntervalUnitMonth MaintenancePlanIntervalUnit = "month"
	MaintenancePlanIntervalUnitYear  MaintenancePlanIntervalUnit = "year"
)

type MaintenancePlanCreate struct {
	CompletedDate       types.Date                  `json:"completedDate"`
	Cost                float64                     `json:"cost,string"`
	Name                string                      `json:"name"                          validate:"required"`
	Description         string                      `json:"description"`
	IntervalValue       int                         `json:"intervalValue"                 validate:"required,min=1"`
	IntervalUnit        MaintenancePlanIntervalUnit `json:"intervalUnit"                  validate:"required"`
	StartDate           types.Date                  `json:"startDate"`
	Active              bool                        `json:"active"`
	LinkExistingEntryID *uuid.UUID                  `json:"linkExistingEntryID,omitempty"`
}

type MaintenancePlanUpdate struct {
	Name          string                      `json:"name"`
	Description   string                      `json:"description"`
	IntervalValue int                         `json:"intervalValue"`
	IntervalUnit  MaintenancePlanIntervalUnit `json:"intervalUnit"`
	NextDueAt     *types.Date                 `json:"nextDueAt,omitempty"`
	Active        bool                        `json:"active"`
}

type MaintenancePlan struct {
	ID              uuid.UUID                   `json:"id"`
	ItemID          uuid.UUID                   `json:"itemID"`
	Name            string                      `json:"name"`
	Description     string                      `json:"description"`
	IntervalValue   int                         `json:"intervalValue"`
	IntervalUnit    MaintenancePlanIntervalUnit `json:"intervalUnit"`
	Active          bool                        `json:"active"`
	LastCompletedAt time.Time                   `json:"lastCompletedAt"`
	NextDueAt       time.Time                   `json:"nextDueAt"`
}

func (mc MaintenancePlanCreate) Validate() error {
	base := mc.CompletedDate.Time()
	if base.IsZero() {
		base = mc.StartDate.Time()
	}
	return validateMaintenanceInterval(base, mc.IntervalValue, mc.IntervalUnit)
}

func (mu MaintenancePlanUpdate) Validate() error {
	base := time.Time{}
	if mu.NextDueAt != nil {
		base = mu.NextDueAt.Time()
	}
	return validateMaintenanceInterval(base, mu.IntervalValue, mu.IntervalUnit)
}

func validateMaintenanceInterval(base time.Time, value int, unit MaintenancePlanIntervalUnit) error {
	// Bound arithmetic before multiplying. Calendar dates are four-digit years.
	maximum := 0
	switch unit {
	case MaintenancePlanIntervalUnitHour:
		maximum = 24 * 366 * 9999
		if value%24 != 0 {
			return errors.New("hour intervals must be whole days (multiples of 24)")
		}
	case MaintenancePlanIntervalUnitDay:
		maximum = 366 * 9999
	case MaintenancePlanIntervalUnitWeek:
		maximum = 53 * 9999
	case MaintenancePlanIntervalUnitMonth:
		maximum = 12 * 9999
	case MaintenancePlanIntervalUnitYear:
		maximum = 9999
	default:
		return errors.New("invalid intervalUnit")
	}
	if value < 1 || value > maximum {
		return errors.New("intervalValue is outside the supported range")
	}
	if base.IsZero() {
		base = maintenanceToday()
	}
	next := computeNextDue(base.UTC(), value, unit)
	if next.Year() > 9999 || next.Year() < 1 || !next.After(base) {
		return errors.New("next maintenance date is outside the supported range")
	}
	return nil
}

func mapMaintenancePlan(entry *ent.MaintenancePlan) MaintenancePlan {
	last := time.Time{}
	next := time.Time{}
	if entry.LastCompletedAt != nil {
		last = entry.LastCompletedAt.UTC()
	}
	if entry.NextDueAt != nil {
		next = entry.NextDueAt.UTC()
	}

	return MaintenancePlan{
		ID:              entry.ID,
		ItemID:          entry.EntityID,
		Name:            entry.Name,
		Description:     entry.Description,
		IntervalValue:   entry.IntervalValue,
		IntervalUnit:    MaintenancePlanIntervalUnit(entry.IntervalUnit),
		Active:          entry.Active,
		LastCompletedAt: last,
		NextDueAt:       next,
	}
}

func (r *MaintenanceEntryRepository) ListPlansByItemID(ctx context.Context, groupID, itemID uuid.UUID) ([]MaintenancePlan, error) {
	items, err := r.db.MaintenancePlan.Query().
		Where(
			maintenanceplan.HasEntityWith(
				entity.IDEQ(itemID),
				entity.HasGroupWith(group.IDEQ(groupID)),
			),
		).
		Order(ent.Asc(maintenanceplan.FieldName)).
		All(ctx)
	if err != nil {
		return nil, err
	}

	return mapEach(items, mapMaintenancePlan), nil
}

// ownedEntity reports whether itemID belongs to the given group.
func ownedEntity(ctx context.Context, db *ent.Client, gid, itemID uuid.UUID) (bool, error) {
	return db.Entity.Query().
		Where(entity.ID(itemID), entity.HasGroupWith(group.ID(gid))).
		Exist(ctx)
}

// ownedPlan returns the plan only if it belongs to an entity in the given group.
func ownedPlan(ctx context.Context, db *ent.Client, gid, planID uuid.UUID) (*ent.MaintenancePlan, error) {
	return db.MaintenancePlan.Query().
		Where(
			maintenanceplan.ID(planID),
			maintenanceplan.HasEntityWith(entity.HasGroupWith(group.ID(gid))),
		).
		Only(ctx)
}

// lockMaintenancePlan acquires a transactional write lock without changing the
// interval. Both SQLite and PostgreSQL support this; acquire it before touching
// an occurrence so completion and plan editing use the same lock order.
func lockMaintenancePlan(ctx context.Context, db *ent.Client, planID uuid.UUID) error {
	return db.MaintenancePlan.UpdateOneID(planID).AddIntervalValue(0).Exec(ctx)
}

// withTx runs fn inside a transaction, committing on success and rolling
// back on error.
func (r *MaintenanceEntryRepository) withTx(ctx context.Context, fn func(db *ent.Client) error) error {
	tx, err := r.db.Tx(ctx)
	if err != nil {
		return err
	}

	defer func() { _ = tx.Rollback() }()

	if err := fn(tx.Client()); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil {
			return fmt.Errorf("%w (rollback failed: %w)", err, rbErr)
		}
		return err
	}

	return tx.Commit()
}

func (r *MaintenanceEntryRepository) CreatePlan(ctx context.Context, gid, itemID uuid.UUID, input MaintenancePlanCreate) (MaintenancePlan, error) {
	owned, err := ownedEntity(ctx, r.db, gid, itemID)
	if err != nil {
		return MaintenancePlan{}, err
	}
	if !owned {
		return MaintenancePlan{}, &ent.NotFoundError{}
	}

	if err := input.Validate(); err != nil {
		return MaintenancePlan{}, validate.NewFieldErrors(validate.NewFieldError("intervalValue", err.Error()))
	}

	base := input.StartDate.Time()
	if base.IsZero() {
		base = time.Now().UTC()
	}
	firstDue := types.DateFromTime(base.UTC()).Time()

	var plan *ent.MaintenancePlan
	err = r.withTx(ctx, func(db *ent.Client) error {
		var err error
		plan, err = db.MaintenancePlan.Create().
			SetEntityID(itemID).
			SetName(input.Name).
			SetDescription(input.Description).
			SetIntervalValue(input.IntervalValue).
			SetIntervalUnit(maintenanceplan.IntervalUnit(input.IntervalUnit)).
			SetActive(input.Active).
			SetNextDueAt(firstDue).
			Save(ctx)
		if err != nil {
			return err
		}

		if input.LinkExistingEntryID != nil && *input.LinkExistingEntryID != uuid.Nil {
			// Only entries on the same entity can be linked; the entity is
			// already known to belong to the caller's group.
			existing, err := db.MaintenanceEntry.Query().
				Where(
					maintenanceentry.IDEQ(*input.LinkExistingEntryID),
					maintenanceentry.EntityIDEQ(itemID),
				).
				Only(ctx)
			if err != nil {
				return fmt.Errorf("link existing maintenance entry: %w", err)
			}
			if existing.PlanID != nil {
				return validate.NewFieldErrors(validate.NewFieldError("linkExistingEntryID", "entry already belongs to a plan"))
			}
			link := db.MaintenanceEntry.UpdateOneID(existing.ID).Where(maintenanceentry.PlanIDIsNil()).SetPlanID(plan.ID)
			if existing.Date.IsZero() {
				link.Where(maintenanceentry.Or(maintenanceentry.DateIsNil(), maintenanceentry.DateEQ(time.Time{})))
			} else {
				link.Where(maintenanceentry.DateEQ(existing.Date))
			}
			if existing.ScheduledDate.IsZero() {
				link.Where(maintenanceentry.Or(maintenanceentry.ScheduledDateIsNil(), maintenanceentry.ScheduledDateEQ(time.Time{})))
			} else {
				link.Where(maintenanceentry.ScheduledDateEQ(existing.ScheduledDate))
			}
			if _, err = link.Save(ctx); err != nil {
				return err
			}
			if !existing.Date.IsZero() {
				if err := rollPlanFromCompletion(ctx, db, plan.ID, existing.Date.UTC(), itemID); err != nil {
					return err
				}
			} else {
				due := existing.ScheduledDate
				if due.IsZero() {
					due = firstDue
					if err := db.MaintenanceEntry.UpdateOneID(existing.ID).SetScheduledDate(due).Exec(ctx); err != nil {
						return err
					}
				}
				if err := db.MaintenancePlan.UpdateOneID(plan.ID).SetNextDueAt(due).Exec(ctx); err != nil {
					return err
				}
			}
			plan, err = db.MaintenancePlan.Get(ctx, plan.ID)
			return err
		}

		_, err = db.MaintenanceEntry.Create().
			SetEntityID(itemID).
			SetPlanID(plan.ID).
			SetName(plan.Name).
			SetDescription(plan.Description).
			SetScheduledDate(firstDue).
			SetDate(input.CompletedDate.Time()).
			SetCost(input.Cost).
			Save(ctx)
		if err != nil {
			return err
		}
		if !input.CompletedDate.Time().IsZero() {
			if err := rollPlanFromCompletion(ctx, db, plan.ID, input.CompletedDate.Time(), itemID); err != nil {
				return err
			}
			plan, err = db.MaintenancePlan.Get(ctx, plan.ID)
		}
		return err
	})
	if err != nil {
		return MaintenancePlan{}, err
	}

	return mapMaintenancePlan(plan), nil
}

func (r *MaintenanceEntryRepository) UpdatePlan(ctx context.Context, gid, planID uuid.UUID, input MaintenancePlanUpdate) (MaintenancePlan, error) {
	var plan *ent.MaintenancePlan
	err := r.withTx(ctx, func(db *ent.Client) error {
		if _, err := ownedPlan(ctx, db, gid, planID); err != nil {
			return err
		}
		if err := lockMaintenancePlan(ctx, db, planID); err != nil {
			return err
		}
		current, err := ownedPlan(ctx, db, gid, planID)
		if err != nil {
			return err
		}
		if err := input.Validate(); err != nil {
			return validate.NewFieldErrors(validate.NewFieldError("intervalValue", err.Error()))
		}
		if input.NextDueAt != nil && input.NextDueAt.Time().IsZero() {
			return validate.NewFieldErrors(validate.NewFieldError("nextDueAt", "next due date cannot be empty"))
		}
		up := db.MaintenancePlan.UpdateOneID(planID).SetName(input.Name).SetDescription(input.Description).
			SetIntervalValue(input.IntervalValue).SetIntervalUnit(maintenanceplan.IntervalUnit(input.IntervalUnit)).SetActive(input.Active)
		if input.NextDueAt != nil {
			due := input.NextDueAt.Time()
			up.SetNextDueAt(due)
			_, err := db.MaintenanceEntry.Update().Where(maintenanceentry.PlanIDEQ(planID), maintenanceentry.Or(maintenanceentry.DateIsNil(), maintenanceentry.DateEQ(time.Time{}))).SetScheduledDate(due).Save(ctx)
			if err != nil {
				return err
			}
		}
		plan, err = up.Save(ctx)
		if err != nil {
			return err
		}
		// A plan completed while paused has no pending entry. Resuming must create it.
		if input.Active && !current.Active && current.LastCompletedAt != nil {
			count, err := db.MaintenanceEntry.Query().Where(maintenanceentry.PlanIDEQ(planID), maintenanceentry.Or(maintenanceentry.DateIsNil(), maintenanceentry.DateEQ(time.Time{}))).Count(ctx)
			if err != nil {
				return err
			}
			if count == 0 {
				if input.NextDueAt != nil {
					_, err = db.MaintenanceEntry.Create().SetEntityID(current.EntityID).SetPlanID(planID).
						SetName(plan.Name).SetDescription(plan.Description).SetDate(time.Time{}).
						SetScheduledDate(input.NextDueAt.Time()).Save(ctx)
				} else {
					err = rollPlanFromCompletion(ctx, db, planID, current.LastCompletedAt.UTC(), current.EntityID)
				}
				if err != nil {
					return err
				}
				plan, err = db.MaintenancePlan.Get(ctx, planID)
				if err != nil {
					return err
				}
			}
		}
		return err
	})
	if err != nil {
		return MaintenancePlan{}, err
	}
	return mapMaintenancePlan(plan), nil
}

func (r *MaintenanceEntryRepository) DeletePlan(ctx context.Context, gid, planID uuid.UUID) error {
	n, err := r.db.MaintenancePlan.Delete().
		Where(
			maintenanceplan.ID(planID),
			maintenanceplan.HasEntityWith(entity.HasGroupWith(group.ID(gid))),
		).
		Exec(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return &ent.NotFoundError{}
	}

	return nil
}

// validatePlanForEntity ensures planID refers to a plan on the given entity.
// Entries may only be attached to plans on their own entity; this also keeps
// plans from other groups out of reach.
func validatePlanForEntity(ctx context.Context, db *ent.Client, planID, entityID uuid.UUID) error {
	exists, err := db.MaintenancePlan.Query().
		Where(maintenanceplan.ID(planID), maintenanceplan.EntityIDEQ(entityID)).
		Exist(ctx)
	if err != nil {
		return err
	}
	if !exists {
		return &ent.NotFoundError{}
	}

	return nil
}

// rollPlanFromCompletion advances a plan after one of its entries is completed
// and creates the next open entry. The caller must have verified that the plan
// belongs to entityID.
func rollPlanFromCompletion(ctx context.Context, db *ent.Client, planID uuid.UUID, completedAt time.Time, entityID uuid.UUID) error {
	if err := lockMaintenancePlan(ctx, db, planID); err != nil {
		return err
	}
	plan, err := db.MaintenancePlan.Query().
		Where(maintenanceplan.ID(planID), maintenanceplan.EntityIDEQ(entityID)).
		Only(ctx)
	if err != nil {
		return err
	}

	if !plan.Active {
		// Paused plans record the completion but do not schedule a follow-up.
		return db.MaintenancePlan.UpdateOneID(planID).
			SetLastCompletedAt(completedAt).
			Exec(ctx)
	}

	if err := validateMaintenanceInterval(completedAt, plan.IntervalValue, MaintenancePlanIntervalUnit(plan.IntervalUnit)); err != nil {
		return validate.NewFieldErrors(validate.NewFieldError("completedDate", err.Error()))
	}
	nextDue := computeNextDue(completedAt.UTC(), plan.IntervalValue, MaintenancePlanIntervalUnit(plan.IntervalUnit))
	updated, err := db.MaintenancePlan.UpdateOneID(planID).
		SetLastCompletedAt(completedAt).
		SetNextDueAt(nextDue).
		Save(ctx)
	if err != nil {
		return err
	}

	openCount, err := db.MaintenanceEntry.Query().
		Where(
			maintenanceentry.PlanIDEQ(planID),
			maintenanceentry.ScheduledDateEQ(nextDue),
			maintenanceentry.Or(
				maintenanceentry.DateIsNil(),
				maintenanceentry.DateEQ(time.Time{}),
			),
		).
		Count(ctx)
	if err != nil {
		return err
	}

	if openCount > 0 {
		return nil
	}

	return db.MaintenanceEntry.Create().
		SetEntityID(entityID).
		SetPlanID(planID).
		SetName(updated.Name).
		SetDescription(updated.Description).
		SetScheduledDate(nextDue).
		SetDate(time.Time{}).
		Exec(ctx)
}

// addMonthsClamped adds n months to t, clamping the day to the last day of the
// target month so that e.g. Jan 31 + 1 month is Feb 28/29 rather than Mar 3.
func addMonthsClamped(t time.Time, n int) time.Time {
	y, m, d := t.Date()
	firstOfTarget := time.Date(y, m+time.Month(n), 1, t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), t.Location())
	lastDay := firstOfTarget.AddDate(0, 1, -1).Day()
	if d > lastDay {
		d = lastDay
	}

	return time.Date(firstOfTarget.Year(), firstOfTarget.Month(), d, t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), t.Location())
}

func computeNextDue(base time.Time, intervalValue int, intervalUnit MaintenancePlanIntervalUnit) time.Time {
	switch intervalUnit {
	case MaintenancePlanIntervalUnitHour:
		return base.AddDate(0, 0, intervalValue/24).Add(time.Duration(intervalValue%24) * time.Hour)
	case MaintenancePlanIntervalUnitDay:
		return base.AddDate(0, 0, intervalValue)
	case MaintenancePlanIntervalUnitWeek:
		return base.AddDate(0, 0, 7*intervalValue)
	case MaintenancePlanIntervalUnitMonth:
		return addMonthsClamped(base, intervalValue)
	case MaintenancePlanIntervalUnitYear:
		return addMonthsClamped(base, 12*intervalValue)
	default:
		return base
	}
}
