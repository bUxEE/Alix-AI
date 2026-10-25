<script setup lang="ts">
import { computed } from "vue";
import { useAlix } from "../../i18n";
import type { TableBlock, TableCell } from "../../sdk-types";

// A table of plain values. Every cell is text: the renderer never interprets a cell as markup or as a link. Numbers align to
// the end and use tabular figures; a missing value is a dash, announced as "no value" so a screen reader does not read a bare dash.

const MAX_ROWS = 500;

const props = defineProps<{
  block: TableBlock;
}>();

const { t, formatNumber } = useAlix();

const rows = computed(() => props.block.rows.slice(0, MAX_ROWS));
const hidden = computed(() => Math.max(0, props.block.rows.length - MAX_ROWS));

const show = (cell: TableCell | undefined, kind: string | undefined): string => {
  if (cell === null || cell === undefined) return "";
  if (typeof cell === "boolean") return cell ? t("common.yes") : t("common.no");
  if (typeof cell === "number" && kind === "number") return formatNumber(cell);
  return String(cell);
};
</script>

<template>
  <div class="alix-root alix-table-wrap">
    <table>
      <caption v-if="block.caption">
        {{ block.caption }}
      </caption>
      <thead>
        <tr>
          <th
            v-for="column in block.columns"
            :key="column.key"
            :class="{ 'alix-num': column.kind === 'number' }"
            scope="col"
          >
            {{ column.label }}
          </th>
        </tr>
      </thead>
      <tbody>
        <tr v-if="rows.length === 0">
          <td
            :colspan="Math.max(1, block.columns.length)"
            class="alix-text-muted"
          >
            {{ t("table.empty") }}
          </td>
        </tr>
        <tr
          v-for="(row, rowIndex) in rows"
          :key="rowIndex"
        >
          <td
            v-for="(column, columnIndex) in block.columns"
            :key="column.key"
            :class="{ 'alix-num': column.kind === 'number' }"
          >
            <template v-if="row[columnIndex] === null || row[columnIndex] === undefined">
              <span aria-hidden="true">
                &ndash;
              </span>
              <span class="alix-sr-only">
                {{ t("table.noValue") }}
              </span>
            </template>
            <template v-else>
              {{ show(row[columnIndex], column.kind) }}
            </template>
          </td>
        </tr>
      </tbody>
    </table>
    <p
      v-if="hidden > 0"
      class="alix-text-muted alix-text-sm alix-pa-2 alix-ma-0"
    >
      {{ t("table.more", { n: hidden }) }}
    </p>
  </div>
</template>
