<template>
  <div class="external-db-query-display">
    <div v-if="data.truncated" class="truncated-note">
      {{ $t('chat.externalQueryTruncated') }}
    </div>
    <div v-if="data.rows && data.rows.length > 0" class="results-table-container">
      <table class="results-table">
        <thead>
          <tr>
            <th v-for="(column, index) in data.columns" :key="index">{{ column }}</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="(row, rowIndex) in data.rows" :key="rowIndex">
            <td v-for="(cell, cellIndex) in row" :key="cellIndex">{{ cell }}</td>
          </tr>
        </tbody>
      </table>
    </div>
    <div v-else class="no-results">
      {{ $t('chat.noDatabaseRecords') }}
    </div>
  </div>
</template>

<script setup lang="ts">
import type { ExternalDBQueryData } from '@/types/tool-results';

interface Props {
  data: ExternalDBQueryData;
}

defineProps<Props>();
</script>

<style lang="less" scoped>
.external-db-query-display {
  font-size: var(--app-text-md);
  color: var(--td-text-color-primary);
}

.truncated-note {
  padding: 10px 12px;
  background: var(--td-warning-color-light);
  border-left: 3px solid var(--td-warning-color);
  border-radius: var(--app-radius-xs);
  margin-bottom: 12px;
  font-size: var(--app-text-md);
}

.results-table-container {
  overflow-x: auto;
  border: 1px solid var(--td-component-stroke);
  border-radius: var(--app-radius-sm);
  background: var(--td-bg-color-container);
}

.results-table {
  width: 100%;
  border-collapse: collapse;
  font-size: var(--app-text-sm);

  thead {
    background: var(--td-bg-color-secondarycontainer);
    border-bottom: 2px solid var(--td-component-stroke);

    th {
      padding: 10px 12px;
      text-align: left;
      font-weight: 600;
      color: var(--td-text-color-primary);
      white-space: nowrap;
    }
  }

  tbody {
    tr {
      border-bottom: 1px solid var(--td-component-stroke);

      &:hover {
        background: var(--td-bg-color-secondarycontainer);
      }

      &:last-child {
        border-bottom: none;
      }
    }

    td {
      padding: 10px 12px;
      color: var(--td-text-color-primary);
      vertical-align: top;
      max-width: 400px;
      overflow: hidden;
      text-overflow: ellipsis;
    }
  }
}

.no-results {
  padding: 32px;
  text-align: center;
  color: var(--td-text-color-placeholder);
  font-style: italic;
  background: var(--td-bg-color-secondarycontainer);
  border-radius: var(--app-radius-sm);
  border: 1px solid var(--td-component-stroke);
}
</style>
