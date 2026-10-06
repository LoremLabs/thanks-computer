---
title: Thanks, Computer
description: 'Deploy your Nuxt Application to Thanks, Computer.'
logoSrc: '/assets/integrations/thanks-computer.svg'
category: Hosting
nitroPreset: 'thanks-computer'
website: 'https://www.thanks.computer'
---

Install the preset, and point Nitro at it:

```bash [Terminal]
npm install -D @txco/nitro-preset
```

```ts [nuxt.config.ts]
export default defineNuxtConfig({
  nitro: {
    preset: '@txco/nitro-preset'
  }
})
```

## Deploy a prerendered site

```bash [Terminal]
npx nuxi generate
```

The build lands in `txco-web/`. Bind it to a stack in your workspace's `txco.yaml`, check it, and deploy with the [txco CLI](https://www.thanks.computer):

```yaml [txco.yaml]
stacks:
  web:
    abi: txco-web
```

```bash [Terminal]
txco web check txco-web
txco apply
```

A page that doesn't exist gets your `404.html` with status 404; an app with `ssr: false` gets its shell with status 200, so client routes still render.

::tip
Add `txco-web/` to `.gitignore`: it's build output, regenerated on every build.
::

## Learn more

:read-more{to="https://www.npmjs.com/package/@txco/nitro-preset" title="@txco/nitro-preset" target="_blank"}
