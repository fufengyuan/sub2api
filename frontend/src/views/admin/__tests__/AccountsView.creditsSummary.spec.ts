import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

import AccountsView from '../AccountsView.vue'

const { listAccounts } = vi.hoisted(() => ({
  listAccounts: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      list: listAccounts,
      listWithEtag: vi.fn(),
      getBatchTodayStats: vi.fn().mockResolvedValue({ stats: {} }),
      getUpstreamBillingProbeSettings: vi.fn().mockResolvedValue({ enabled: true, interval_minutes: 30 }),
      delete: vi.fn(),
      batchClearError: vi.fn(),
      batchRefresh: vi.fn(),
      toggleSchedulable: vi.fn()
    },
    proxies: { getAll: vi.fn().mockResolvedValue([]) },
    groups: { getAll: vi.fn().mockResolvedValue([]) }
  }
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError: vi.fn(), showSuccess: vi.fn(), showInfo: vi.fn() })
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({ token: 'test-token', isSimpleMode: false })
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({ t: (key: string, params?: Record<string, unknown>) => {
      if (params && 'count' in params) return `${key}:${params.count}`
      return key
    } })
  }
})

function mountView() {
  return mount(AccountsView, {
    global: {
      stubs: {
        AppLayout: { template: '<div><slot /></div>' },
        TablePageLayout: {
          template: '<div><slot name="filters" /><slot name="table" /><slot name="pagination" /></div>'
        },
        // 用真实 DataTable：footer 汇总行渲染在 DataTable 内部，stub 掉就测不到了。
        AccountTableActions: { template: '<div><slot name="after" /></div>' },
        AccountTableFilters: true,
        AccountBulkActionsBar: true,
        Pagination: true,
        ConfirmDialog: true,
        AccountActionMenu: true,
        ImportDataModal: true,
        ReAuthAccountModal: true,
        AccountTestModal: true,
        AccountStatsModal: true,
        ScheduledTestsPanel: true,
        SyncFromCrsModal: true,
        TempUnschedStatusModal: true,
        ErrorPassthroughRulesModal: true,
        TLSFingerprintProfilesModal: true,
        CreateAccountModal: true,
        EditAccountModal: true,
        BulkEditAccountModal: true,
        PlatformTypeBadge: true,
        AccountCapacityCell: true,
        AccountStatusIndicator: true,
        AccountTodayStatsCell: true,
        AccountGroupsCell: true,
        AccountUsageCell: true,
        HelpTooltip: true,
        Icon: true,
        Teleport: true
      }
    }
  })
}

describe('admin AccountsView CN credits summary footer', () => {
  beforeEach(() => {
    localStorage.clear()
    listAccounts.mockReset()
  })

  it('sums creditsRemain across CN upstream accounts only', async () => {
    listAccounts.mockResolvedValue({
      items: [
        { id: 1, platform: 'workbuddy', credentials: { creditsRemain: 1200 } },
        { id: 2, platform: 'traework', credentials: { creditsRemain: 300 } },
        { id: 3, platform: 'qwenwork', credentials: { creditsRemain: 45.5 } },
        { id: 4, platform: 'openai', credentials: { creditsRemain: 99999 } },
        { id: 5, platform: 'qoder' },
        { id: 6, platform: 'claude' }
      ],
      total: 6,
      page: 1,
      page_size: 20,
      pages: 1
    })

    const wrapper = mountView()
    await flushPromises()

    const summary = wrapper.find('[data-test="cn-credits-summary"]')
    expect(summary.exists()).toBe(true)
    // 1200 + 300 + 45.5 = 1545.5；openai 的 99999 与无积分的 qoder/claude 不计入。
    expect(summary.text()).toContain('1,545.5')
    expect(summary.text()).toContain('admin.accounts.cnCredits.summary:3')
  })

  it('hides the summary row when no CN account has credits', async () => {
    listAccounts.mockResolvedValue({
      items: [
        { id: 1, platform: 'openai' },
        { id: 2, platform: 'qoder' }
      ],
      total: 2,
      page: 1,
      page_size: 20,
      pages: 1
    })

    const wrapper = mountView()
    await flushPromises()

    expect(wrapper.find('[data-test="cn-credits-summary"]').exists()).toBe(false)
  })
})
