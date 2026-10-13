import type {SidebarsConfig} from '@docusaurus/plugin-content-docs';
const sidebars: SidebarsConfig = {
  docs: [
    'index',
    {type: 'category', label: 'Getting started', collapsed: false, items: ['quick-start', 'concepts', 'install']},
    {type: 'category', label: 'Reference', collapsed: false, items: [
      'providers', 'api-keys', 'presets', 'budgets', 'routing', 'offboarding', 'alerts', 'upgrading',
    ]},
  ],
};
export default sidebars;
