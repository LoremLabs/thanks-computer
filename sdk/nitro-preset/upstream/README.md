# Upstream drafts

Prepared, not opened. They're the route to a listing on nuxt.com/deploy.

- **`nitro-thanks-computer.patch`:** the `thanks-computer` and
  `thanks-computer-static` presets for Nitro `main` (Nitro 3), with a docs page
  and a preset test. It is based on nitrojs/nitro `6bb682e`; apply it with
  `git apply`. `NITRO-PR.md` is the PR text. Before opening, run
  `pnpm gen-presets` and `pnpm vitest test/presets/thanks-computer.test.ts` in
  the branch; neither has been run.
- **`nuxt.com/content/deploy/thanks-computer.md`:** the nuxt.com deploy page.
  `NUXT-COM-PR.md` is the PR text. It also needs
  `public/assets/integrations/thanks-computer.svg`: a small logo that works on
  light and dark backgrounds.

Delete this directory once both are submitted.
