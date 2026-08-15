import { createI18n } from 'vue-i18n'
import zhCN from './locales/zh-CN'
import en from './locales/en'

// i18n wiring (FR-023 groundwork): V1.0 ships zh-CN first with the en
// dictionary stubbed; every UI string MUST go through a translation key,
// never hardcoded, so layout already reserves ~1.5x width for English.
export const i18n = createI18n({
  legacy: false,
  locale: 'zh-CN',
  fallbackLocale: 'en',
  messages: { 'zh-CN': zhCN, en },
})
