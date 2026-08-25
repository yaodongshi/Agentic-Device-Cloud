#!/usr/bin/env node

import fs from 'node:fs'
import path from 'node:path'
import vm from 'node:vm'
import { fileURLToPath } from 'node:url'

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const defaultEnglish = path.join(root, 'ce/console/src/i18n/locales/en.ts')
const defaultChinese = path.join(root, 'ce/console/src/i18n/locales/zh-CN.ts')
const allowlistPath = path.join(root, 'scripts/i18n-term-allowlist.json')

function loadResource(file) {
  const source = fs.readFileSync(file, 'utf8')
  if (file.endsWith('.json')) return JSON.parse(source)
  const expression = source.replace(/^\s*export\s+default\s+/, '').trim().replace(/;$/, '')
  return vm.runInNewContext(`(${expression})`, Object.create(null), { filename: file })
}

function flatten(value, prefix = '', output = new Map()) {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) {
    output.set(prefix, value)
    return output
  }
  for (const key of Object.keys(value)) flatten(value[key], prefix ? `${prefix}.${key}` : key, output)
  return output
}

function placeholders(value) {
  if (typeof value !== 'string') return []
  return [...value.matchAll(/\{([A-Za-z_][A-Za-z0-9_.-]*)\}/g)].map((match) => match[1]).sort()
}

function valueType(value) {
  if (Array.isArray(value)) return 'array'
  if (value === null) return 'null'
  return typeof value
}

function validate(englishFile, chineseFile, { terminology = true } = {}) {
  const en = flatten(loadResource(englishFile))
  const zh = flatten(loadResource(chineseFile))
  const errors = []
  const allKeys = new Set([...en.keys(), ...zh.keys()])

  for (const key of [...allKeys].sort()) {
    if (!en.has(key)) {
      errors.push(`${key}: 英文资源缺少 key`)
      continue
    }
    if (!zh.has(key)) {
      errors.push(`${key}: 中文资源缺少 key`)
      continue
    }
    const enValue = en.get(key)
    const zhValue = zh.get(key)
    if (valueType(enValue) !== valueType(zhValue)) {
      errors.push(`${key}: 值类型不一致（en=${valueType(enValue)}, zh=${valueType(zhValue)}）`)
    }
    if (typeof enValue === 'string' && (!enValue.trim() || !String(zhValue).trim())) {
      errors.push(`${key}: 翻译值不能为空`)
    }
    if (placeholders(enValue).join(',') !== placeholders(zhValue).join(',')) {
      errors.push(`${key}: 占位符不一致（en={${placeholders(enValue)}}, zh={${placeholders(zhValue)}}）`)
    }
  }

  if (terminology) {
    const allowlist = new Set(JSON.parse(fs.readFileSync(allowlistPath, 'utf8')).keys)
    const rules = [
      [/\bcredential(s)?\b/i, /凭证/, 'Credential/凭证'],
      [/\bpermission(s)?\b/i, /权限/, 'Permission/权限'],
      [/\bapproval(s)?|approver|approved?\b/i, /审批|同意/, 'Approval/审批'],
      [/\bhigh-risk\b/i, /高危/, 'High-risk/高危'],
      [/\brevoke|revocation|revoked\b/i, /吊销/, 'Revoke/吊销'],
      [/\bexecute|executed|execution\b/i, /执行/, 'Execute/执行'],
      [/\bdispatch|dispatched\b/i, /下发/, 'Dispatch/下发'],
    ]
    for (const [key, enValue] of en) {
      if (allowlist.has(key) || typeof enValue !== 'string' || typeof zh.get(key) !== 'string') continue
      for (const [sourcePattern, targetPattern, term] of rules) {
        if (sourcePattern.test(enValue) && !targetPattern.test(zh.get(key))) {
          errors.push(`${key}: 高风险术语 ${term} 未使用审核译法；如确需例外，加入 scripts/i18n-term-allowlist.json`)
        }
      }
    }
  }
  return errors
}

function runSelfTest() {
  const fixtureRoot = path.join(root, 'scripts/fixtures/i18n')
  const cases = [
    ['missing-key', '中文资源缺少 key'],
    ['type-mismatch', '值类型不一致'],
    ['placeholder-mismatch', '占位符不一致'],
    ['empty-value', '翻译值不能为空'],
  ]
  for (const [name, expected] of cases) {
    const errors = validate(
      path.join(fixtureRoot, `${name}.en.json`),
      path.join(fixtureRoot, `${name}.zh.json`),
      { terminology: false },
    )
    if (!errors.some((error) => error.includes(expected))) {
      throw new Error(`失败样例 ${name} 未触发预期错误：${expected}`)
    }
  }
  console.log(`i18n 失败样例自测通过（${cases.length} 项）`)
}

const args = process.argv.slice(2)
if (args.includes('--self-test')) runSelfTest()
const files = args.filter((arg) => arg !== '--self-test')
const errors = validate(files[0] ? path.resolve(files[0]) : defaultEnglish, files[1] ? path.resolve(files[1]) : defaultChinese)
if (errors.length) {
  console.error(`i18n 校验失败（${errors.length} 项）：`)
  for (const error of errors) console.error(`- ${error}`)
  process.exit(1)
}
console.log('i18n 校验通过：key、类型、占位符、空值和高风险术语一致')
