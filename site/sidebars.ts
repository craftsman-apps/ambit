import type {SidebarsConfig} from '@docusaurus/plugin-content-docs';
const sidebars: SidebarsConfig = {
  docs: [
    'index',
    {type: 'category', label: 'Getting started', collapsed: false, items: ['quick-start', 'local-up', 'concepts', 'install']},
    {type: 'category', label: 'Guides', collapsed: false, items: [
      'guides/coding-tools', 'guides/codex-opencode-hermes', 'guides/other-agents', 'guides/models-per-team', 'guides/mcp-apps', 'guides/request-an-app', 'guides/claude-desktop', 'guides/claude-desktop-mcp', 'guides/spending-limits', 'guides/someone-leaves',
    ]},
    {type: 'category', label: 'Reference', collapsed: false, items: [
      'cli', 'providers', 'api-keys', 'presets', 'budgets', 'routing', 'offboarding', 'alerts', 'compliance', 'upgrading',
    ]},
  ],
};
export default sidebars;
