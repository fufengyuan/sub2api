import { describe, expect, it, vi, afterEach } from 'vitest'
import { mount } from '@vue/test-utils'
import { nextTick } from 'vue'
import AccountActionMenu from '../AccountActionMenu.vue'
import type { Account } from '@/types'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key,
    }),
  }
})

function makeAccount(overrides: Partial<Account> = {}): Account {
  return {
    id: 1,
    name: 'test-account',
    platform: 'traework',
    type: 'oauth',
    proxy_id: null,
    concurrency: 3,
    priority: 50,
    status: 'active',
    error_message: null,
    last_used_at: null,
    expires_at: null,
    auto_pause_on_expired: false,
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
    schedulable: true,
    rate_limited_at: null,
    rate_limit_reset_at: null,
    overload_until: null,
    temp_unschedulable_until: null,
    temp_unschedulable_reason: null,
    session_window_start: null,
    session_window_end: null,
    session_window_status: null,
    ...overrides,
  } as Account
}

// Teleport 渲染到 document.body；菜单容器带 action-menu-content 类。
const getMenuEl = () => document.body.querySelector('.action-menu-content') as HTMLElement | null

function mockMenuRect(width: number, height: number) {
  const original = HTMLElement.prototype.getBoundingClientRect
  vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect').mockImplementation(function (this: HTMLElement) {
    if (this.classList?.contains('action-menu-content')) {
      return { width, height } as DOMRect
    }
    return original.call(this)
  })
}

async function flushReposition() {
  // 组件内部 reposition 含一次 await nextTick，测试侧再等一轮微任务队列。
  await nextTick()
  await nextTick()
}

describe('AccountActionMenu — 视口自适应定位', () => {
  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('末行菜单底部溢出视口时上移，保证完整展示', async () => {
    // jsdom 默认 innerHeight=768 / innerWidth=1024。
    // 菜单实测高 500：top=600 时 600+500=1100 > 768-8 → 修正为 768-500-8=260。
    mockMenuRect(208, 500)
    const wrapper = mount(AccountActionMenu, {
      props: { show: true, account: makeAccount(), position: { top: 600, left: 100 } },
      attachTo: document.body,
    })
    await flushReposition()

    const el = getMenuEl()
    expect(el).not.toBeNull()
    expect(el!.style.top).toBe('260px')
    expect(el!.style.left).toBe('100px')
    wrapper.unmount()
  })

  it('右侧溢出时向左收，底部不溢出时保持原位', async () => {
    // 菜单宽 208：left=950 时 950+208=1158 > 1024-8 → 修正为 1024-208-8=808；
    // top=100 且高 300：100+300=400 < 760 → 保持 100。
    mockMenuRect(208, 300)
    const wrapper = mount(AccountActionMenu, {
      props: { show: true, account: makeAccount(), position: { top: 100, left: 950 } },
      attachTo: document.body,
    })
    await flushReposition()

    const el = getMenuEl()
    expect(el!.style.top).toBe('100px')
    expect(el!.style.left).toBe('808px')
    wrapper.unmount()
  })

  it('菜单高于视口时贴顶并依赖 max-h 滚动兜底', async () => {
    // 高 800 > 768-16：修正后 top 至少为 EDGE_PADDING=8（配合容器 max-h 滚动）。
    mockMenuRect(208, 800)
    const wrapper = mount(AccountActionMenu, {
      props: { show: true, account: makeAccount(), position: { top: 700, left: 100 } },
      attachTo: document.body,
    })
    await flushReposition()

    const el = getMenuEl()
    expect(el!.style.top).toBe('8px')
    wrapper.unmount()
  })
})
