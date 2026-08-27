<template>
  <BaseDialog
    :show="show"
    :title="t('admin.accounts.cnCredits.detailTitle')"
    width="normal"
    @close="handleClose"
  >
    <div class="space-y-4">
      <div v-if="loading" class="flex items-center justify-center gap-2 py-8 text-sm text-gray-500 dark:text-gray-400">
        <svg class="h-5 w-5 animate-spin" fill="none" viewBox="0 0 24 24">
          <circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4"></circle>
          <path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4zm2 5.291A7.962 7.962 0 014 12H0c0 3.042 1.135 5.824 3 7.938l3-2.647z"></path>
        </svg>
        {{ t('admin.accounts.cnCredits.loading') }}
      </div>

      <template v-else>
        <div class="rounded-lg border border-gray-200 p-4 dark:border-dark-600">
          <p class="text-xs text-gray-500 dark:text-gray-400">{{ account?.name || '-' }}</p>
          <p class="mt-2 flex items-baseline gap-2">
            <span class="text-2xl font-semibold text-gray-900 dark:text-gray-100">{{ detail?.credits_remain ?? '-' }}</span>
            <span class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.accounts.cnCredits.detailRemain') }}</span>
          </p>
        </div>

        <div v-if="detail && detail.items.length > 0" class="overflow-x-auto rounded-lg border border-gray-200 dark:border-dark-600">
          <table class="w-full text-sm">
            <thead>
              <tr class="border-b border-gray-200 bg-gray-50 text-left text-xs text-gray-500 dark:border-dark-600 dark:bg-dark-700 dark:text-gray-400">
                <th class="px-3 py-2 font-medium">{{ t('admin.accounts.cnCredits.detailItemName') }}</th>
                <th class="px-3 py-2 text-right font-medium">{{ t('admin.accounts.cnCredits.detailTotal') }}</th>
                <th class="px-3 py-2 text-right font-medium">{{ t('admin.accounts.cnCredits.detailUsed') }}</th>
                <th class="px-3 py-2 text-right font-medium">{{ t('admin.accounts.cnCredits.detailRemainCol') }}</th>
                <th class="px-3 py-2 font-medium">{{ t('admin.accounts.cnCredits.detailExpiry') }}</th>
              </tr>
            </thead>
            <tbody>
              <tr
                v-for="(item, index) in detail.items"
                :key="index"
                class="border-b border-gray-100 last:border-0 dark:border-dark-700"
              >
                <td class="px-3 py-2 text-gray-900 dark:text-gray-100">{{ item.name || '-' }}</td>
                <td class="px-3 py-2 text-right font-mono text-gray-700 dark:text-gray-300">{{ item.total }}</td>
                <td class="px-3 py-2 text-right font-mono text-gray-700 dark:text-gray-300">{{ item.used }}</td>
                <td class="px-3 py-2 text-right font-mono font-medium text-emerald-600 dark:text-emerald-400">{{ item.remain }}</td>
                <td class="px-3 py-2 text-gray-500 dark:text-gray-400">{{ item.expiry || '-' }}</td>
              </tr>
            </tbody>
          </table>
        </div>

        <div v-else class="rounded-lg border border-gray-200 p-4 text-sm text-gray-500 dark:border-dark-600 dark:text-gray-400">
          {{ t('admin.accounts.cnCredits.detailEmpty') }}
        </div>
      </template>
    </div>

    <template #footer>
      <div class="flex justify-end gap-3">
        <button type="button" class="btn btn-secondary" :disabled="loading" @click="loadDetail">
          <svg class="mr-1 h-4 w-4" :class="[loading ? 'animate-spin' : '']" fill="none" viewBox="0 0 24 24" stroke="currentColor" stroke-width="1.5">
            <path stroke-linecap="round" stroke-linejoin="round" d="M16.023 9.348h4.992v-.001M2.985 19.644v-4.992m0 0h4.992m-4.993 0l3.181 3.183a8.25 8.25 0 0013.803-3.7M4.031 9.865a8.25 8.25 0 0113.803-3.7l3.181 3.182m0-4.991v4.99" />
          </svg>
          {{ t('common.refresh') }}
        </button>
        <button type="button" class="btn btn-primary" @click="handleClose">
          {{ t('common.close') }}
        </button>
      </div>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import { adminAPI } from '@/api/admin'
import type { Account } from '@/types'
import type { CnCreditsDetail } from '@/api/admin/accounts'
import BaseDialog from '@/components/common/BaseDialog.vue'

const props = defineProps<{
  show: boolean
  account: Account | null
}>()

const emit = defineEmits<{
  close: []
}>()

const { t } = useI18n()
const appStore = useAppStore()

const loading = ref(false)
const detail = ref<CnCreditsDetail | null>(null)

const loadDetail = async () => {
  if (!props.account) return
  loading.value = true
  try {
    detail.value = await adminAPI.accounts.getCnCreditsDetail(props.account.id)
  } catch (error: any) {
    appStore.showError(error?.message || String(error))
    detail.value = null
  } finally {
    loading.value = false
  }
}

const handleClose = () => {
  emit('close')
}

watch(
  () => [props.show, props.account?.id],
  ([visible]) => {
    if (visible && props.account) {
      loadDetail()
      return
    }
    detail.value = null
  }
)
</script>
