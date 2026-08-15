<template>
  <div class="page">
    <div class="card">
      <!-- loading -->
      <template v-if="state === 'loading'">
        <div class="icon-wrap pending">
          <svg
            viewBox="0 0 24 24"
            class="icon"
          >
            <circle
              cx="12"
              cy="12"
              r="9"
              fill="none"
              stroke="currentColor"
              stroke-width="2"
              stroke-dasharray="42"
              stroke-dashoffset="14"
            />
          </svg>
        </div>
        <h2 class="title">
          {{ t('hitl.action.loading') }}
        </h2>
      </template>

      <!-- confirm: signature valid and ticket still pending -->
      <template v-else-if="state === 'confirm'">
        <div class="icon-wrap pending">
          <svg
            viewBox="0 0 24 24"
            class="icon"
          >
            <path
              d="M12 2 1 21h22L12 2z"
              fill="none"
              stroke="currentColor"
              stroke-width="2"
              stroke-linejoin="round"
            />
            <line
              x1="12"
              y1="9"
              x2="12"
              y2="14"
              stroke="currentColor"
              stroke-width="2"
            />
            <circle
              cx="12"
              cy="17"
              r="1"
              fill="currentColor"
            />
          </svg>
        </div>
        <h2 class="title">
          {{ t('hitl.action.confirmTitle') }}
        </h2>
        <p class="subtitle">
          {{ decisionLabel }}
        </p>
        <dl class="info">
          <div class="row">
            <dt>{{ t('hitl.action.ticketId') }}</dt>
            <dd class="mono">
              {{ ticket?.ticket_id }}
            </dd>
          </div>
          <div
            v-if="ticket?.device_code"
            class="row"
          >
            <dt>{{ t('hitl.action.device') }}</dt>
            <dd class="mono">
              {{ ticket?.device_code }}
            </dd>
          </div>
          <div
            v-if="ticket?.tool_name"
            class="row"
          >
            <dt>{{ t('hitl.action.tool') }}</dt>
            <dd class="mono">
              {{ ticket?.tool_name }}
            </dd>
          </div>
          <div
            v-if="ticket?.risk_level !== undefined"
            class="row"
          >
            <dt>{{ t('hitl.action.risk') }}</dt>
            <dd>{{ t(`tools.riskLevels.${ticket?.risk_level}`) }}</dd>
          </div>
        </dl>
        <p class="note">
          {{ t('hitl.action.confirmHint') }}
        </p>
        <el-button
          class="primary-btn"
          :class="decision === 'approve' ? 'approve-btn' : 'reject-btn'"
          :loading="submitting"
          @click="submit"
        >
          {{ decision === 'approve' ? t('hitl.action.confirmExecute') : t('hitl.action.confirmReject') }}
        </el-button>
      </template>

      <!-- success -->
      <template v-else-if="state === 'success'">
        <div class="icon-wrap success">
          <svg
            viewBox="0 0 24 24"
            class="icon"
          >
            <path
              d="M4 12.5 9.5 18 20 6"
              fill="none"
              stroke="currentColor"
              stroke-width="2.5"
              stroke-linecap="round"
              stroke-linejoin="round"
            />
          </svg>
        </div>
        <h2 class="title">
          {{ t('hitl.action.successTitle') }}
        </h2>
        <dl class="info">
          <div class="row">
            <dt>{{ t('hitl.action.ticketId') }}</dt>
            <dd class="mono">
              {{ ticketId }}
            </dd>
          </div>
          <div class="row">
            <dt>{{ t('hitl.action.result') }}</dt>
            <dd>{{ decision === 'approve' ? t('hitl.action.approvedResult') : t('hitl.action.rejectedResult') }}</dd>
          </div>
        </dl>
        <el-button
          class="primary-btn"
          @click="goConsole"
        >
          {{ t('hitl.action.backConsole') }}
        </el-button>
      </template>

      <!-- failed: already processed / signature invalid / not found -->
      <template v-else-if="state === 'failed'">
        <div class="icon-wrap failed">
          <svg
            viewBox="0 0 24 24"
            class="icon"
          >
            <path
              d="M12 2 1 21h22L12 2z"
              fill="none"
              stroke="currentColor"
              stroke-width="2"
              stroke-linejoin="round"
            />
            <line
              x1="12"
              y1="9"
              x2="12"
              y2="14"
              stroke="currentColor"
              stroke-width="2"
            />
            <circle
              cx="12"
              cy="17"
              r="1"
              fill="currentColor"
            />
          </svg>
        </div>
        <h2 class="title">
          {{ t('hitl.action.failedTitle') }}
        </h2>
        <p class="subtitle">
          {{ failedReason }}
        </p>
        <el-button
          class="primary-btn"
          @click="goConsole"
        >
          {{ t('hitl.action.backConsole') }}
        </el-button>
        <p class="footnote">
          {{ t('hitl.action.contactAdmin') }}
        </p>
      </template>

      <!-- expired -->
      <template v-else-if="state === 'expired'">
        <div class="icon-wrap expired">
          <svg
            viewBox="0 0 24 24"
            class="icon"
          >
            <circle
              cx="12"
              cy="12"
              r="9"
              fill="none"
              stroke="currentColor"
              stroke-width="2"
            />
            <line
              x1="12"
              y1="7"
              x2="12"
              y2="12"
              stroke="currentColor"
              stroke-width="2"
            />
            <line
              x1="12"
              y1="12"
              x2="16"
              y2="14"
              stroke="currentColor"
              stroke-width="2"
            />
          </svg>
        </div>
        <h2 class="title">
          {{ t('hitl.action.expiredTitle') }}
        </h2>
        <p class="subtitle">
          {{ t('hitl.action.expiredHint') }}
        </p>
        <el-button
          class="primary-btn"
          @click="goConsole"
        >
          {{ t('hitl.action.backConsole') }}
        </el-button>
      </template>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute } from 'vue-router'
import type { HitlActionTicket, HitlDecision } from '@/api/types'

const { t } = useI18n()
const route = useRoute()

// Standalone, session-less callback landing page for IM card buttons
// (design/20 4.14, F-11). Security rests entirely on the URL signature
// plus the ticket state machine's one-shot consumption (SEC-01/13), so the
// shared request wrapper — which injects a session token and redirects to
// /login on 401 — is deliberately not used here.
type PageState = 'loading' | 'confirm' | 'success' | 'failed' | 'expired'

const state = ref<PageState>('loading')
const ticketId = computed(() => String(route.query.ticket_id ?? ''))
const decision = computed<HitlDecision>(() => (route.query.decision === 'reject' ? 'reject' : 'approve'))
const expire = computed(() => Number(route.query.expire ?? 0))
const sig = computed(() => String(route.query.sig ?? ''))

const ticket = ref<HitlActionTicket | null>(null)
const failedReason = ref('')
const submitting = ref(false)

const decisionLabel = computed(() =>
  decision.value === 'approve' ? t('hitl.action.decisionApprove') : t('hitl.action.decisionReject'),
)

// Local fetch: no auth header, no login redirect, JSON only.
async function hitlFetch<T>(url: string, init?: { method?: string; headers?: Record<string, string>; body?: string }): Promise<T> {
  const res = await fetch(url, init)
  const text = await res.text()
  let body: { code?: number; message?: string } = {}
  try {
    body = text ? JSON.parse(text) : {}
  } catch {
    body = {}
  }
  if (!res.ok) {
    const err = new Error(body.message || res.statusText) as Error & { code?: number }
    err.code = body.code
    throw err
  }
  return body as T
}

function goConsole() {
  window.location.href = '/approvals'
}

function failWith(key: string) {
  failedReason.value = t(key)
  state.value = 'failed'
}

onMounted(async () => {
  // Param sanity before any network call.
  if (!ticketId.value || !sig.value) {
    failWith('hitl.action.reasonInvalidSignature')
    return
  }
  // Local expiry check first; the server re-validates authoritatively
  // (design/33 3.4.2: expired clicks render the expired state).
  if (expire.value > 0 && expire.value * 1000 < Date.now()) {
    state.value = 'expired'
    return
  }
  try {
    // Signed validation: GET /v1/hitl/action (design/33 3.4.2). The EE
    // contract 302-redirects to the console confirm page; V1.0 CE returns
    // the pending-ticket summary as JSON so this standalone page can
    // render the confirm state itself.
    const qs = new URLSearchParams({
      ticket_id: ticketId.value,
      decision: decision.value,
      expire: String(expire.value),
      sig: sig.value,
    })
    const res = await hitlFetch<HitlActionTicket>(`/v1/hitl/action?${qs.toString()}`)
    ticket.value = res
    if (res.status === 'pending') {
      state.value = 'confirm'
    } else if (res.status === 'expired') {
      state.value = 'expired'
    } else {
      failWith('hitl.action.reasonProcessed')
    }
  } catch (err) {
    // Map contract error codes to the three states (design/33 3.4.2).
    const code = (err as { code?: number })?.code
    if (code === 12003) {
      state.value = 'expired'
    } else if (code === 12002) {
      failWith('hitl.action.reasonProcessed')
    } else if (code === 12004) {
      failWith('hitl.action.reasonInvalidSignature')
    } else {
      failWith('hitl.action.reasonNotFound')
    }
  }
})

async function submit() {
  if (!ticketId.value) return
  submitting.value = true
  try {
    // Final decision via POST /v1/hitl/callback (design/33 3.4.1).
    // Production callback requests are HMAC-signed with the ticket-level
    // callback secret (design/33 1.4); the browser cannot mint that
    // signature, so V1.0 CE sends the unsigned body and the approval
    // service derives the real approver from the signed ticket context
    // (SEC-01). Replace with the signed flow before production.
    await hitlFetch('/v1/hitl/callback', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        ticket_id: ticketId.value,
        decision: decision.value,
        approver: '',
        comment: '',
      }),
    })
    state.value = 'success'
  } catch (err) {
    const code = (err as { code?: number })?.code
    if (code === 12003) {
      state.value = 'expired'
    } else if (code === 12002) {
      failWith('hitl.action.reasonProcessed')
    } else {
      failWith('hitl.action.reasonNotFound')
    }
  } finally {
    submitting.value = false
  }
}
</script>

<style scoped>
/* Mobile-first standalone page (design/20 4.14): single column, tuned for
   375px wide viewports, 44px touch targets. */
.page {
  min-height: 100vh;
  display: flex;
  align-items: center;
  justify-content: center;
  padding: var(--adc-space-4);
  background: var(--adc-bg);
  box-sizing: border-box;
}
.card {
  width: 100%;
  max-width: 400px;
  background: #fff;
  border: 1px solid var(--adc-border);
  border-radius: 12px;
  padding: var(--adc-space-6);
  text-align: center;
  box-sizing: border-box;
}
.icon-wrap {
  width: 64px;
  height: 64px;
  margin: 0 auto var(--adc-space-4);
  border-radius: 50%;
  display: flex;
  align-items: center;
  justify-content: center;
}
.icon-wrap .icon { width: 36px; height: 36px; }
.icon-wrap.pending { background: rgba(217, 119, 6, 0.12); color: var(--adc-risk-2); }
.icon-wrap.success { background: rgba(22, 163, 74, 0.12); color: var(--adc-risk-0); }
.icon-wrap.failed { background: rgba(217, 119, 6, 0.12); color: var(--adc-risk-2); }
.icon-wrap.expired { background: rgba(107, 114, 128, 0.14); color: var(--adc-text-secondary); }
.title { margin: 0 0 var(--adc-space-2); font-size: 20px; }
.subtitle { margin: 0 0 var(--adc-space-4); color: var(--adc-text-secondary); }
.info { margin: 0 0 var(--adc-space-4); text-align: left; border: 1px solid var(--adc-border); border-radius: var(--adc-radius); padding: var(--adc-space-3); }
.info .row { display: flex; gap: var(--adc-space-2); padding: var(--adc-space-1) 0; }
.info dt { flex: 0 0 96px; color: var(--adc-text-secondary); }
.info dd { margin: 0; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; word-break: break-all; }
.note { margin: 0 0 var(--adc-space-4); color: var(--adc-text-secondary); font-size: 13px; }
.footnote { margin-top: var(--adc-space-4); color: var(--adc-text-secondary); font-size: 12px; }
.primary-btn { width: 100%; min-height: 44px; margin: 0; }
.approve-btn { --el-button-bg-color: var(--adc-risk-0); --el-button-border-color: var(--adc-risk-0); }
.reject-btn { --el-button-bg-color: var(--adc-risk-3); --el-button-border-color: var(--adc-risk-3); }
</style>
