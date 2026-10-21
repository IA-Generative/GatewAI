import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';

export default defineConfig({
  site: 'https://ia-generative.github.io',
  base: '/GatewAI',
  integrations: [
    starlight({
      title: 'GatewAI',
      components: {
        SiteTitle: './src/components/SiteTitle.astro',
        Hero: './src/components/Hero.astro',
        Head: './src/components/Head.astro',
      },
      customCss: ['./src/styles/landing.css'],
      social: [
        { icon: 'github', label: 'GitHub', href: 'https://github.com/IA-Generative/GatewAI' },
      ],
      sidebar: [
        {
          label: 'Get started',
          items: [
            { label: 'Overview',       slug: 'get-started/overview' },
            { label: 'Request flows',  slug: 'get-started/request-flows' },
          ],
        },
        {
          label: 'Set up',
          items: [
            { label: 'Helm chart',        slug: 'set-up/helm' },
            { label: 'Configuration',     slug: 'set-up/config' },
            { label: 'Relay queue',       slug: 'set-up/queue' },
          ],
        },
        {
          label: 'Relay',
          items: [
            { label: 'Async flow', slug: 'relay/async' },
          ],
        },
        {
          label: 'Configure',
          items: [
            { label: 'Configuration reference', slug: 'configure/configuration' },
            { label: 'Authentication',          slug: 'configure/authentication' },
            { label: 'Access control',          slug: 'configure/access-control' },
            { label: 'Service registry',        slug: 'configure/service-registry' },
            { label: 'Priority routing',        slug: 'configure/priority-routing' },
            { label: 'LLM proxy',               slug: 'configure/llm-proxy' },
            { label: 'Rate limiting',           slug: 'configure/rate-limiting' },
            { label: 'PII guardrails',          slug: 'configure/guardrails' },
            { label: 'Audit log',               slug: 'configure/audit-log' },
            { label: 'Encryption',              slug: 'configure/encryption' },
          ],
        },
        {
          label: 'Observe',
          items: [
            { label: 'Prometheus',      slug: 'observe/prometheus' },
            { label: 'OpenTelemetry',   slug: 'observe/opentelemetry' },
            { label: 'Span reference',  slug: 'observe/spans' },
          ],
        },
        {
          label: 'Manage',
          items: [
            { label: 'Upgrade guide', slug: 'manage/upgrade' },
            { label: 'Runbooks',      slug: 'manage/runbooks' },
          ],
        },
        {
          label: 'Reference',
          items: [
            { label: 'API',       slug: 'reference/api' },
            { label: 'Metrics',   slug: 'reference/metrics' },
            { label: 'Changelog', slug: 'reference/changelog' },
          ],
        },
        {
          label: 'Community',
          items: [
            { label: 'Gitflow',   slug: 'community/gitflow' },
            { label: 'Releasing', slug: 'community/releasing' },
          ],
        },
      ],
    }),
  ],
});
