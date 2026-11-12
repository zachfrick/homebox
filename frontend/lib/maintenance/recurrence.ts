import { MaintenancePlanIntervalUnit } from "~~/lib/api/types/data-contracts";

function addInterval(base: Date, intervalValue: number, intervalUnit: MaintenancePlanIntervalUnit): Date {
  const next = new Date(base);
  switch (intervalUnit) {
    case MaintenancePlanIntervalUnit.Hour:
      next.setHours(next.getHours() + intervalValue);
      return next;
    case MaintenancePlanIntervalUnit.Day:
      next.setDate(next.getDate() + intervalValue);
      return next;
    case MaintenancePlanIntervalUnit.Week:
      next.setDate(next.getDate() + intervalValue * 7);
      return next;
    case MaintenancePlanIntervalUnit.Month: {
      const y = next.getFullYear();
      const m = next.getMonth();
      const day = next.getDate();
      const totalMonths = y * 12 + m + intervalValue;
      const ty = Math.floor(totalMonths / 12);
      const tm = totalMonths - ty * 12;
      const lastDay = new Date(ty, tm + 1, 0).getDate();
      next.setFullYear(ty, tm, Math.min(day, lastDay));
      return next;
    }
    case MaintenancePlanIntervalUnit.Year: {
      const y = next.getFullYear() + intervalValue;
      const m = next.getMonth();
      const day = next.getDate();
      const lastDay = new Date(y, m + 1, 0).getDate();
      next.setFullYear(y, m, Math.min(day, lastDay));
      return next;
    }
    default:
      return next;
  }
}

export function getNextNDueDates(
  firstDueDate: Date,
  intervalValue: number,
  intervalUnit: MaintenancePlanIntervalUnit,
  count: number
): Date[] {
  const result: Date[] = [];
  let current = new Date(firstDueDate);
  for (let i = 0; i < count; i++) {
    result.push(new Date(current));
    current = addInterval(current, intervalValue, intervalUnit);
  }
  return result;
}
