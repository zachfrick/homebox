package repo

import (
	"context"
	"errors"
	"time"

	"entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"github.com/sysadminsmedia/homebox/backend/internal/data/ent"
	"github.com/sysadminsmedia/homebox/backend/internal/data/ent/entity"
	"github.com/sysadminsmedia/homebox/backend/internal/data/ent/group"
	"github.com/sysadminsmedia/homebox/backend/internal/data/ent/maintenanceentry"
	"github.com/sysadminsmedia/homebox/backend/internal/data/ent/maintenanceplan"
	"github.com/sysadminsmedia/homebox/backend/internal/data/types"
	"github.com/sysadminsmedia/homebox/backend/internal/sys/validate"
)

// MaintenanceEntryRepository is a repository for maintenance entries that are
// associated with an item in the database. An entry represents a maintenance event
// that has been performed on an item.
type MaintenanceEntryRepository struct {
	db *ent.Client
}

type MaintenanceEntryCreate struct {
	CompletedDate types.Date `json:"completedDate"`
	ScheduledDate types.Date `json:"scheduledDate"`
	PlanID        uuid.UUID  `json:"planID"`
	Name          string     `json:"name"          validate:"required"`
	Description   string     `json:"description"`
	Cost          float64    `json:"cost,string"`
}

func (mc MaintenanceEntryCreate) Validate() error {
	if mc.CompletedDate.Time().IsZero() && mc.ScheduledDate.Time().IsZero() {
		return errors.New("either completedDate or scheduledDate must be set")
	}
	return nil
}

type MaintenanceEntryUpdate struct {
	CompletedDate types.Date `json:"completedDate"`
	ScheduledDate types.Date `json:"scheduledDate"`
	PlanID        *uuid.UUID `json:"planID,omitempty"`
	Name          string     `json:"name"`
	Description   string     `json:"description"`
	Cost          float64    `json:"cost,string"`
}

func (mu MaintenanceEntryUpdate) Validate() error {
	if mu.CompletedDate.Time().IsZero() && mu.ScheduledDate.Time().IsZero() {
		return errors.New("either completedDate or scheduledDate must be set")
	}
	return nil
}

type (
	MaintenanceEntry struct {
		ID            uuid.UUID  `json:"id"`
		CompletedDate types.Date `json:"completedDate"`
		ScheduledDate types.Date `json:"scheduledDate"`
		PlanID        uuid.UUID  `json:"planID,omitempty"`
		IsOverdue     bool       `json:"isOverdue"`
		Name          string     `json:"name"`
		Description   string     `json:"description"`
		Cost          float64    `json:"cost,string"`
	}
)

var (
	mapMaintenanceEntryErr  = mapTErrFunc(mapMaintenanceEntry)
	mapEachMaintenanceEntry = mapTEachFunc(mapMaintenanceEntry)
)

func mapMaintenanceEntry(entry *ent.MaintenanceEntry) MaintenanceEntry {
	planID := uuid.Nil
	if entry.PlanID != nil {
		planID = *entry.PlanID
	}
	return MaintenanceEntry{
		ID:            entry.ID,
		CompletedDate: types.DateFromDBTime(entry.Date),
		ScheduledDate: types.DateFromDBTime(entry.ScheduledDate),
		PlanID:        planID,
		IsOverdue:     isEntryOverdue(entry.Date, entry.ScheduledDate),
		Name:          entry.Name,
		Description:   entry.Description,
		Cost:          entry.Cost,
	}
}

func (r *MaintenanceEntryRepository) GetScheduled(ctx context.Context, gid uuid.UUID, dt types.Date) ([]MaintenanceEntry, error) {
	entries, err := r.db.MaintenanceEntry.Query().
		Where(
			maintenanceentry.HasEntityWith(
				entity.HasGroupWith(group.ID(gid)),
			),
			maintenanceentry.ScheduledDate(dt.Time()),
			maintenanceentry.Or(maintenanceentry.PlanIDIsNil(), maintenanceentry.HasPlanWith(maintenanceplan.Active(true))),
			maintenanceentry.Or(
				maintenanceentry.DateIsNil(),
				maintenanceentry.DateEQ(time.Time{}),
			),
		).
		All(ctx)

	if err != nil {
		return nil, err
	}

	return mapEachMaintenanceEntry(entries), nil
}

// GetOverdueRecurring returns open entries of active maintenance plans whose
// scheduled date is before dt. One-off entries are excluded so existing
// notification behaviour for them is unchanged.
func (r *MaintenanceEntryRepository) GetOverdueRecurring(ctx context.Context, gid uuid.UUID, dt types.Date) ([]MaintenanceEntry, error) {
	entries, err := r.db.MaintenanceEntry.Query().
		Where(
			maintenanceentry.HasEntityWith(
				entity.HasGroupWith(group.ID(gid)),
			),
			maintenanceentry.HasPlanWith(maintenanceplan.Active(true)),
			maintenanceentry.ScheduledDateNotNil(),
			maintenanceentry.ScheduledDateGT(time.Time{}),
			maintenanceentry.ScheduledDateLT(dt.Time()),
			maintenanceentry.Or(
				maintenanceentry.DateIsNil(),
				maintenanceentry.DateEQ(time.Time{}),
			),
		).
		Order(ent.Asc(maintenanceentry.FieldScheduledDate)).
		All(ctx)
	if err != nil {
		return nil, err
	}

	return mapEachMaintenanceEntry(entries), nil
}

func (r *MaintenanceEntryRepository) Create(ctx context.Context, gid, itemID uuid.UUID, input MaintenanceEntryCreate) (MaintenanceEntry, error) {
	// Verify the target item belongs to the caller's group before creating an
	// entry against it. Without this check, a caller in group A could POST a
	// maintenance entry to any item UUID guessed in group B; the entry would
	// then surface in B's maintenance log with no audit trail.
	owned, err := r.db.Entity.Query().
		Where(entity.ID(itemID), entity.HasGroupWith(group.ID(gid))).
		Exist(ctx)
	if err != nil {
		return MaintenanceEntry{}, err
	}
	if !owned {
		return MaintenanceEntry{}, &ent.NotFoundError{}
	}

	create := r.db.MaintenanceEntry.Create().
		SetEntityID(itemID).
		SetDate(input.CompletedDate.Time()).
		SetScheduledDate(input.ScheduledDate.Time()).
		SetName(input.Name).
		SetDescription(input.Description).
		SetCost(input.Cost)

	if input.PlanID != uuid.Nil {
		if err := validatePlanForEntity(ctx, r.db, input.PlanID, itemID); err != nil {
			return MaintenanceEntry{}, err
		}
		return MaintenanceEntry{}, validate.NewFieldErrors(validate.NewFieldError("planID", "use a maintenance plan to create recurring entries"))
	}

	item, err := create.Save(ctx)
	return mapMaintenanceEntryErr(item, err)
}

func (r *MaintenanceEntryRepository) Update(ctx context.Context, gid uuid.UUID, id uuid.UUID, input MaintenanceEntryUpdate) (MaintenanceEntry, error) {
	var item *ent.MaintenanceEntry
	err := r.withTx(ctx, func(db *ent.Client) error {
		current, err := db.MaintenanceEntry.Query().Where(
			maintenanceentry.ID(id), maintenanceentry.HasEntityWith(entity.HasGroupWith(group.ID(gid))),
		).Only(ctx)
		if err != nil {
			return err
		}
		planID := current.PlanID
		if input.PlanID != nil {
			planID = input.PlanID
		}
		linked := planID != nil && *planID != uuid.Nil
		if linked {
			if err := validatePlanForEntity(ctx, db, *planID, current.EntityID); err != nil {
				return err
			}
			if current.PlanID == nil || *current.PlanID != *planID {
				return validate.NewFieldErrors(validate.NewFieldError("planID", "use plan creation to link an existing entry"))
			}
			if err := lockMaintenancePlan(ctx, db, *planID); err != nil {
				return err
			}
			// A successor may already have been completed. Reopening history must not
			// manufacture another occurrence; detach it explicitly first if needed.
			if !current.Date.IsZero() && input.CompletedDate.Time().IsZero() {
				return validate.NewFieldErrors(validate.NewFieldError("completedDate", "detach a recurring entry before reopening it"))
			}
		}
		updater := db.MaintenanceEntry.UpdateOneID(id).
			SetDate(input.CompletedDate.Time()).SetScheduledDate(input.ScheduledDate.Time()).
			SetName(input.Name).SetDescription(input.Description).SetCost(input.Cost)
		// Completion and plan linking must not act on a stale association or date.
		if current.PlanID == nil {
			updater.Where(maintenanceentry.PlanIDIsNil())
		} else {
			updater.Where(maintenanceentry.PlanIDEQ(*current.PlanID))
		}
		if current.Date.IsZero() {
			updater.Where(maintenanceentry.Or(maintenanceentry.DateIsNil(), maintenanceentry.DateEQ(time.Time{})))
		} else {
			updater.Where(maintenanceentry.DateEQ(current.Date))
		}
		if input.PlanID != nil {
			if linked {
				updater.SetPlanID(*planID)
			} else {
				updater.ClearPlanID()
			}
		}
		item, err = updater.Save(ctx)
		if err != nil {
			return err
		}
		if linked && current.Date.IsZero() {
			if !input.CompletedDate.Time().IsZero() {
				return rollPlanFromCompletion(ctx, db, *planID, input.CompletedDate.Time(), item.EntityID)
			}
			// Editing the pending occurrence also moves the plan's next date.
			return db.MaintenancePlan.UpdateOneID(*planID).SetNextDueAt(input.ScheduledDate.Time()).Exec(ctx)
		}
		return nil
	})
	if err != nil {
		return MaintenanceEntry{}, err
	}
	return mapMaintenanceEntry(item), nil
}

func (r *MaintenanceEntryRepository) GetMaintenanceByItemID(ctx context.Context, groupID, itemID uuid.UUID, filters MaintenanceFilters) ([]MaintenanceEntryWithDetails, error) {
	query := r.db.MaintenanceEntry.Query().Where(
		maintenanceentry.EntityID(itemID),
		maintenanceentry.HasEntityWith(
			entity.HasGroupWith(group.IDEQ(groupID)),
		),
	)
	switch filters.Status {
	case MaintenanceFilterStatusScheduled:
		query = query.Where(maintenanceentry.Or(
			maintenanceentry.DateIsNil(),
			maintenanceentry.DateEQ(time.Time{}),
			maintenanceentry.DateGT(time.Now()),
		))
		// Sort scheduled entries by ascending scheduled date
		query = query.Order(
			maintenanceentry.ByScheduledDate(sql.OrderAsc()),
		)
	case MaintenanceFilterStatusCompleted:
		query = query.Where(
			maintenanceentry.Not(maintenanceentry.Or(
				maintenanceentry.DateIsNil(),
				maintenanceentry.DateEQ(time.Time{}),
				maintenanceentry.DateGT(time.Now()),
			)))
		// Sort completed entries by descending completion date
		query = query.Order(
			maintenanceentry.ByDate(sql.OrderDesc()),
		)
	case MaintenanceFilterStatusOverdue:
		query = query.Where(
			maintenanceentry.ScheduledDateGT(time.Time{}),
			maintenanceentry.ScheduledDateLT(maintenanceToday()),
			maintenanceentry.Or(
				maintenanceentry.DateIsNil(),
				maintenanceentry.DateEQ(time.Time{}),
			),
		)
		query = query.Order(
			maintenanceentry.ByScheduledDate(sql.OrderAsc()),
		)
	default:
		// Sort entries by default by scheduled and maintenance date in descending order
		query = query.Order(
			maintenanceentry.ByScheduledDate(sql.OrderDesc()),
			maintenanceentry.ByDate(sql.OrderDesc()),
		)
	}
	entries, err := query.WithEntity().All(ctx)

	if err != nil {
		return []MaintenanceEntryWithDetails{}, err
	}

	return mapEachMaintenanceEntryWithDetails(entries), nil
}

func isEntryOverdue(completedDate, scheduledDate time.Time) bool {
	if scheduledDate.IsZero() {
		return false
	}

	if completedDate.IsZero() {
		return scheduledDate.Before(maintenanceToday())
	}

	return false
}

func (r *MaintenanceEntryRepository) Delete(ctx context.Context, gid uuid.UUID, id uuid.UUID) error {
	deleted, err := r.db.MaintenanceEntry.Delete().Where(
		maintenanceentry.ID(id),
		maintenanceentry.HasEntityWith(entity.HasGroupWith(group.ID(gid))),
	).Exec(ctx)
	if err != nil {
		return err
	}
	if deleted == 0 {
		return &ent.NotFoundError{}
	}
	return nil
}

// Calendar dates are stored as UTC midnight; use the same cutoff as notifications.
func maintenanceToday() time.Time { return types.DateFromTime(time.Now()).Time() }
