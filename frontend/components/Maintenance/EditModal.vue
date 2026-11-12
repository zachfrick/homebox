<template>
  <Dialog :dialog-id="DialogID.EditMaintenance">
    <DialogContent>
      <DialogHeader>
        <DialogTitle>
          {{ entry.id ? $t("maintenance.modal.edit_title") : $t("maintenance.modal.new_title") }}
        </DialogTitle>
      </DialogHeader>

      <form @submit.prevent="dispatchFormSubmit">
        <p v-if="planLoadFailed" role="alert">
          {{ $t("maintenance.modal.plan_load_failed") }}
        </p>
        <Button v-if="planLoadFailed" type="button" @click="hydrateRecurringPlan">{{
          $t("maintenance.modal.retry")
        }}</Button>
        <fieldset class="flex flex-col gap-2" :disabled="planLoading || planLoadFailed || saving">
          <FormTextField v-model="entry.name" autofocus :label="$t('maintenance.modal.entry_name')" />
          <DatePicker v-model="entry.completedDate" :label="$t('maintenance.modal.completed_date')" />
          <DatePicker v-model="entry.scheduledDate" :label="$t('maintenance.modal.scheduled_date')" />
          <label class="flex items-center gap-2 text-sm">
            <input v-model="entry.isRecurring" type="checkbox" :true-value="true" :false-value="false" />
            {{ $t("maintenance.modal.recurring") }}
          </label>
          <fieldset v-if="entry.isRecurring" :disabled="isLinkedHistory" class="grid grid-cols-2 gap-2">
            <FormTextField v-model="entry.intervalValue" :label="$t('maintenance.modal.interval_value')" />
            <select v-model="entry.intervalUnit" class="rounded-md border p-2 text-sm">
              <option :value="MaintenancePlanIntervalUnit.Day">
                {{ $t("maintenance.modal.interval_day") }}
              </option>
              <option :value="MaintenancePlanIntervalUnit.Week">
                {{ $t("maintenance.modal.interval_week") }}
              </option>
              <option :value="MaintenancePlanIntervalUnit.Month">
                {{ $t("maintenance.modal.interval_month") }}
              </option>
              <option :value="MaintenancePlanIntervalUnit.Year">
                {{ $t("maintenance.modal.interval_year") }}
              </option>
            </select>
          </fieldset>
          <div v-if="entry.isRecurring && nextDuePreview.length > 0" class="rounded-md border p-3 text-sm">
            <p class="font-medium">
              {{ $t("maintenance.modal.next_scheduled_dates") }}
            </p>
            <ul class="mt-1 space-y-1">
              <li v-for="(dueDate, index) in nextDuePreview" :key="index">
                {{ dueDate.toLocaleDateString() }}
              </li>
            </ul>
          </div>
          <FormTextArea v-model="entry.description" :label="$t('maintenance.modal.notes')" />
          <FormTextField v-model="entry.cost" autofocus :label="$t('maintenance.modal.cost')" />

          <DialogFooter>
            <Button type="submit" :disabled="planLoading || planLoadFailed || saving">
              <MdiPost />
              {{ entry.id ? $t("maintenance.modal.edit_action") : $t("maintenance.modal.new_action") }}
            </Button>
          </DialogFooter>
        </fieldset>
      </form>
    </DialogContent>
  </Dialog>
</template>

<script setup lang="ts">
  import { useI18n } from "vue-i18n";
  import { DialogID } from "@/components/ui/dialog-provider/utils";
  import { toast } from "@/components/ui/sonner";
  import MdiPost from "~icons/mdi/post";
  import DatePicker from "~~/components/Form/DatePicker.vue";
  import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
  import { useDialog } from "@/components/ui/dialog-provider";
  import FormTextField from "~/components/Form/TextField.vue";
  import FormTextArea from "~/components/Form/TextArea.vue";
  import { MaintenancePlanIntervalUnit } from "~~/lib/api/types/data-contracts";
  import type { MaintenanceEntryWithDetails, MaintenancePlan } from "~~/lib/api/types/data-contracts";
  import { getNextNDueDates } from "~/lib/maintenance/recurrence";
  import { parseDateOnly, toDateOnlyString } from "~/lib/datelib/dateOnly";
  import Button from "@/components/ui/button/Button.vue";

  const { closeDialog, registerOpenDialogCallback } = useDialog();

  const { t } = useI18n();
  const api = useUserApi();

  const entry = reactive({
    id: null as string | null,
    name: "",
    completedDate: "",
    scheduledDate: "",
    description: "",
    cost: "",
    planID: null as string | null,
    isRecurring: false,
    intervalValue: "30",
    intervalUnit: MaintenancePlanIntervalUnit.Day,
    itemIds: null as string[] | null,
    itemIdForPlanLookup: null as string | null,
  });

  const planLoading = ref(false);
  const planLoadFailed = ref(false);
  const saving = ref(false);
  const loadedPlan = ref<MaintenancePlan | null>(null);
  const originalCompletedDate = ref("");
  const isLinkedHistory = computed(() => !!entry.planID && !!originalCompletedDate.value);
  let planLoadGeneration = 0;

  const planFieldBaseline = reactive({
    name: "",
    description: "",
    intervalValue: "30",
    intervalUnit: MaintenancePlanIntervalUnit.Day,
  });
  function capturePlanFieldBaseline() {
    planFieldBaseline.name = entry.name;
    planFieldBaseline.description = entry.description;
    planFieldBaseline.intervalValue = entry.intervalValue;
    planFieldBaseline.intervalUnit = entry.intervalUnit;
  }
  function planSettingsChanged() {
    return (
      entry.name !== planFieldBaseline.name ||
      entry.description !== planFieldBaseline.description ||
      entry.intervalValue !== planFieldBaseline.intervalValue ||
      entry.intervalUnit !== planFieldBaseline.intervalUnit ||
      entry.isRecurring !== loadedPlan.value?.active
    );
  }

  const nextDuePreview = computed(() => {
    if (!entry.isRecurring) {
      return [];
    }

    const intervalValue = Math.max(parseInt(entry.intervalValue, 10) || 1, 1);
    if (isLinkedHistory.value && loadedPlan.value) {
      const due = parseDateOnly(String(loadedPlan.value.nextDueAt).slice(0, 10));
      return due ? getNextNDueDates(due, intervalValue, entry.intervalUnit, 3) : [];
    }
    const completed = parseDateOnly(entry.completedDate);
    const baseDate = completed ?? parseDateOnly(entry.scheduledDate) ?? new Date();
    const dates = getNextNDueDates(baseDate, intervalValue, entry.intervalUnit, completed ? 4 : 3);
    return completed ? dates.slice(1) : dates;
  });

  function getItemIdFromEntry(maintenanceEntry: unknown): string | null {
    if (!maintenanceEntry || typeof maintenanceEntry !== "object") {
      return null;
    }

    const candidate = (maintenanceEntry as MaintenanceEntryWithDetails).itemID;
    return typeof candidate === "string" && candidate.length > 0 ? candidate : null;
  }

  function normalizePlanId(planId: unknown): string | null {
    if (typeof planId !== "string") {
      return null;
    }

    const normalized = planId.trim().toLowerCase();
    if (!normalized || normalized === "00000000-0000-0000-0000-000000000000") {
      return null;
    }

    return planId;
  }

  async function hydrateRecurringPlan() {
    const generation = ++planLoadGeneration;
    const planID = entry.planID;
    planLoading.value = !!planID;
    planLoadFailed.value = false;
    try {
      if (!planID) {
        capturePlanFieldBaseline();
        return;
      }
      if (!entry.itemIdForPlanLookup) {
        throw new Error("Missing item for maintenance plan");
      }
      const { data, error } = await api.items.maintenance.getPlans(entry.itemIdForPlanLookup);
      if (generation !== planLoadGeneration) return;
      const plan = data?.find(plan => plan.id === planID);
      if (error || !plan) throw new Error("Maintenance plan could not be loaded");
      loadedPlan.value = plan;
      entry.isRecurring = plan.active;
      entry.intervalValue = plan.intervalValue.toString();
      entry.intervalUnit = plan.intervalUnit;
      capturePlanFieldBaseline();
    } catch {
      if (generation === planLoadGeneration) planLoadFailed.value = true;
    } finally {
      if (generation === planLoadGeneration) planLoading.value = false;
    }
  }

  async function dispatchFormSubmit() {
    if (planLoading.value || planLoadFailed.value || saving.value) return;
    saving.value = true;
    try {
      if (entry.id) await editEntry();
      else await createEntry();
    } catch {
      toast.error(t(entry.id ? "maintenance.toast.failed_to_update" : "maintenance.toast.failed_to_create"));
    } finally {
      saving.value = false;
    }
  }

  async function createEntry() {
    if (!entry.itemIds) {
      return;
    }

    const isRecurring = entry.isRecurring === true;

    const itemIds = [...entry.itemIds];
    const results = await Promise.allSettled(
      itemIds.map(async itemId => {
        if (isRecurring) {
          const firstDueDate = entry.scheduledDate || entry.completedDate || toDateOnlyString(new Date());
          const { error: planError } = await api.items.maintenance.createPlan(itemId, {
            name: entry.name,
            description: entry.description,
            active: true,
            intervalValue: parseInt(entry.intervalValue, 10) || 1,
            intervalUnit: entry.intervalUnit,
            startDate: firstDueDate,
            completedDate: entry.completedDate,
            cost: entry.cost || "0",
          });

          if (planError) {
            toast.error(t("maintenance.toast.failed_to_create"));
          }

          return !planError;
        }

        const { error } = await api.items.maintenance.create(itemId, {
          name: entry.name,
          completedDate: entry.completedDate,
          scheduledDate: entry.scheduledDate,
          description: entry.description,
          cost: parseFloat(entry.cost) ? entry.cost : "0",
        });

        if (error) {
          toast.error(t("maintenance.toast.failed_to_create"));
          return false;
        }
        return true;
      })
    );

    // Keep failed items selected so retrying cannot duplicate successful creates.
    entry.itemIds = itemIds.filter(
      (_, index) => results[index]?.status !== "fulfilled" || !(results[index] as PromiseFulfilledResult<boolean>).value
    );
    if (entry.itemIds.length === 0) closeDialog(DialogID.EditMaintenance, true);
  }

  async function editEntry() {
    if (!entry.id) {
      return;
    }

    // Update only deliberately edited plan settings, before completing the entry.
    // Completion alone must never send the old scheduled date back as nextDueAt.
    if (
      entry.planID &&
      loadedPlan.value &&
      (isLinkedHistory.value ? entry.isRecurring !== loadedPlan.value.active : planSettingsChanged())
    ) {
      const { data, error } = await api.maintenance.updatePlan(entry.planID, {
        name: isLinkedHistory.value ? loadedPlan.value.name : entry.name,
        description: isLinkedHistory.value ? loadedPlan.value.description : entry.description,
        active: entry.isRecurring,
        intervalValue: isLinkedHistory.value ? loadedPlan.value.intervalValue : Number(entry.intervalValue),
        intervalUnit: isLinkedHistory.value ? loadedPlan.value.intervalUnit : entry.intervalUnit,
      });
      if (error || !data) {
        toast.error(t("maintenance.toast.failed_to_update"));
        return;
      }
      loadedPlan.value = data;
      capturePlanFieldBaseline();
    }

    const { error } = await api.maintenance.update(entry.id, {
      name: entry.name,
      completedDate: entry.completedDate,
      scheduledDate: entry.scheduledDate,
      planID: entry.planID ?? undefined,
      description: entry.description,
      cost: entry.cost || "0",
    });
    if (error) {
      toast.error(t("maintenance.toast.failed_to_update"));
      return;
    }

    // Save the final historical fields first; conversion rolls from this completion,
    // not from the date that happened to be stored when the dialog opened.
    if (entry.isRecurring && !entry.planID && entry.itemIdForPlanLookup) {
      const { data, error } = await api.items.maintenance.createPlan(entry.itemIdForPlanLookup, {
        name: entry.name,
        description: entry.description,
        active: true,
        intervalValue: Number(entry.intervalValue),
        intervalUnit: entry.intervalUnit,
        startDate: entry.scheduledDate || entry.completedDate || toDateOnlyString(new Date()),
        linkExistingEntryID: entry.id,
      });
      if (error || !data) {
        toast.error(t("maintenance.toast.failed_to_update"));
        return;
      }
      entry.planID = data.id;
    }

    closeDialog(DialogID.EditMaintenance, true);
  }

  onMounted(() => {
    const cleanup = registerOpenDialogCallback(DialogID.EditMaintenance, params => {
      ++planLoadGeneration;
      loadedPlan.value = null;
      planLoading.value = false;
      planLoadFailed.value = false;
      originalCompletedDate.value = params.type === "update" ? String(params.maintenanceEntry.completedDate || "") : "";
      switch (params.type) {
        case "create":
          entry.id = null;
          entry.name = "";
          entry.completedDate = "";
          entry.scheduledDate = "";
          entry.description = "";
          entry.cost = "";
          entry.isRecurring = false;
          entry.intervalValue = "30";
          entry.intervalUnit = MaintenancePlanIntervalUnit.Day;
          entry.planID = null;
          entry.itemIds = typeof params.itemId === "string" ? [params.itemId] : params.itemId;
          entry.itemIdForPlanLookup = typeof params.itemId === "string" ? params.itemId : null;
          break;
        case "update":
          entry.id = params.maintenanceEntry.id;
          entry.name = params.maintenanceEntry.name;
          entry.completedDate = (params.maintenanceEntry.completedDate as string) ?? "";
          entry.scheduledDate = (params.maintenanceEntry.scheduledDate as string) ?? "";
          entry.description = params.maintenanceEntry.description;
          entry.cost = params.maintenanceEntry.cost;
          entry.planID = normalizePlanId(params.maintenanceEntry.planID);
          entry.isRecurring = !!entry.planID;
          entry.intervalValue = "30";
          entry.intervalUnit = MaintenancePlanIntervalUnit.Day;
          entry.itemIdForPlanLookup = params.itemId ?? getItemIdFromEntry(params.maintenanceEntry);
          entry.itemIds = null;
          if (!entry.planID) {
            capturePlanFieldBaseline();
          }
          void hydrateRecurringPlan();
          break;
        case "duplicate":
          entry.id = null;
          entry.name = params.maintenanceEntry.name;
          entry.completedDate = "";
          entry.scheduledDate = "";
          entry.description = params.maintenanceEntry.description;
          entry.cost = params.maintenanceEntry.cost;
          entry.planID = null;
          entry.isRecurring = false;
          entry.itemIds = [params.itemId];
          entry.itemIdForPlanLookup = params.itemId;
          break;
      }
    });

    onUnmounted(cleanup);
  });
</script>
