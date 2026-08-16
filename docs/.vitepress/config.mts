import { defineConfig } from 'vitepress'

const github = 'https://github.com/yaodongshi/Agentic-Device-Cloud'

// A3.2: base path for GitHub Pages. Local builds default to '/'; the docs
// workflow passes VITEPRESS_BASE (configure-pages base_path, e.g. '/<repo>')
// so the site works under project pages. Nav/sidebar links starting with
// '/' are rewritten with this base automatically by the VitePress theme.
const base = process.env.VITEPRESS_BASE ?? '/'

const enNav = [
  { text: 'Home', link: '/en/' },
  { text: 'Quick Start', link: '/en/quickstart' },
  { text: 'Architecture', link: '/en/architecture' },
  { text: 'API Reference', link: '/en/api-reference' },
  { text: 'Open Source', link: '/en/open-source' },
]

const zhNav = [
  { text: '首页', link: '/zh/' },
  { text: '快速开始', link: '/zh/quickstart' },
  { text: '架构', link: '/zh/architecture' },
  { text: '开源', link: '/zh/open-source' },
]

const enSidebar = [
  {
    text: 'Overview',
    items: [
      { text: 'Introduction', link: '/en/' },
      { text: 'Quick Start', link: '/en/quickstart' },
      { text: 'Architecture', link: '/en/architecture' },
    ],
  },
  {
    text: 'Developer Guides',
    items: [
      { text: 'API Reference', link: '/en/api-reference' },
      { text: 'Device SDK', link: '/en/device-sdk' },
    ],
  },
  {
    text: 'Community',
    items: [
      { text: 'Open Source', link: '/en/open-source' },
      { text: 'Roadmap', link: '/en/roadmap' },
    ],
  },
]

const zhSidebar = [
  {
    text: '概览',
    items: [
      { text: '介绍', link: '/zh/' },
      { text: '快速开始', link: '/zh/quickstart' },
      { text: '架构', link: '/zh/architecture' },
    ],
  },
  {
    text: '开发者指南',
    items: [
      { text: 'API 参考', link: '/zh/api-reference' },
      { text: '设备 SDK', link: '/zh/device-sdk' },
    ],
  },
  {
    text: '社区',
    items: [
      { text: '开源', link: '/zh/open-source' },
      { text: '路线图', link: '/zh/roadmap' },
    ],
  },
]

export default defineConfig({
  title: 'ADC Docs',
  description: 'Agentic Device Cloud (ADC) documentation: AI-native device orchestration and governance platform',
  base,
  locales: {
    root: {
      label: 'English',
      lang: 'en-US',
      link: '/en/',
      title: 'ADC Docs',
      description: 'Agentic Device Cloud (ADC) documentation',
    },
    zh: {
      label: '简体中文',
      lang: 'zh-CN',
      link: '/zh/',
      title: 'ADC 文档',
      description: 'Agentic Device Cloud (ADC) 官方文档',
    },
  },
  themeConfig: {
    search: {
      provider: 'local',
    },
    socialLinks: [{ icon: 'github', link: github }],
    locales: {
      root: {
        label: 'English',
        lang: 'en-US',
        nav: enNav,
        sidebar: enSidebar,
        outline: { label: 'On this page' },
        docFooter: {
          prev: 'Previous page',
          next: 'Next page',
        },
        darkModeSwitchLabel: 'Appearance',
        sidebarMenuLabel: 'Menu',
        returnToTopLabel: 'Return to top',
        lastUpdated: { text: 'Last updated' },
      },
      zh: {
        label: '简体中文',
        lang: 'zh-CN',
        nav: zhNav,
        sidebar: zhSidebar,
        outline: { label: '本页目录' },
        docFooter: {
          prev: '上一页',
          next: '下一页',
        },
        darkModeSwitchLabel: '外观',
        sidebarMenuLabel: '菜单',
        returnToTopLabel: '返回顶部',
        lastUpdated: { text: '最后更新' },
      },
    },
  },
})
