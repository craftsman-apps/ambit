import type {Config} from '@docusaurus/types';
import type * as Preset from '@docusaurus/preset-classic';

const config: Config = {
  title: 'Ambit',
  tagline: 'The AI access layer for enterprises.',
  url: 'https://ambitproject.dev',
  baseUrl: '/',
  trailingSlash: false,
  favicon: 'brand/ambit-icon.svg',
  organizationName: 'craftsman-apps',
  projectName: 'ambit',
  onBrokenLinks: 'throw',
  onBrokenAnchors: 'throw',
  markdown: {format: 'detect', hooks: {onBrokenMarkdownLinks: 'throw'}},   // .md files are plain CommonMark: the docs contain <vendor>-style text MDX rejects
  staticDirectories: ['static', '../docs/assets'],   // serves /brand/*.svg for the landing page
  i18n: {defaultLocale: 'en', locales: ['en']},
  presets: [[
    'classic',
    {
      docs: {
        path: '../docs',
        routeBasePath: 'docs',
        sidebarPath: './sidebars.ts',
        exclude: ['maintainers/**', 'superpowers/**', 'assets/**'],
        editUrl: ({docPath}) => `https://github.com/craftsman-apps/ambit/edit/main/docs/${docPath}`,
      },
      blog: false,
      theme: {customCss: './src/css/custom.css'},
      sitemap: {changefreq: 'weekly'},
    } satisfies Preset.Options,
  ]],
  themes: [[
    '@easyops-cn/docusaurus-search-local',
    {hashed: true, docsDir: '../docs', docsRouteBasePath: 'docs', indexBlog: false, indexPages: true},
  ]],
  themeConfig: {
    image: 'img/ambit-social.png',
    metadata: [{name: 'description', content: 'Ambit is a self-hosted gateway that controls a company\'s use of AI models and MCP servers.'}],
    colorMode: {respectPrefersColorScheme: true, disableSwitch: false},   // owner: dark mode with a switch
    navbar: {
      logo: {alt: 'Ambit', src: 'brand/ambit-logo.svg', srcDark: 'brand/ambit-logo-dark.svg'},
      items: [
        {type: 'docSidebar', sidebarId: 'docs', position: 'left', label: 'Docs'},
        {href: 'https://github.com/craftsman-apps/ambit', label: 'GitHub', position: 'right'},
      ],
    },
    footer: {
      style: 'light',
      links: [
        {title: 'Project', items: [
          {label: 'GitHub', href: 'https://github.com/craftsman-apps/ambit'},
          {label: 'Security policy', href: 'https://github.com/craftsman-apps/ambit/security/policy'},
          {label: 'License (Apache-2.0)', href: 'https://github.com/craftsman-apps/ambit/blob/main/LICENSE'},
        ]},
      ],
      copyright: 'Ambit is Alpha software. Apache-2.0.',
    },
  } satisfies Preset.ThemeConfig,
};
export default config;
