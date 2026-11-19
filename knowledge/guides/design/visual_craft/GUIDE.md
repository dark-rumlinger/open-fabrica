---
slug: visual_craft
role: capability
title: Visual craft & richness (any page, app, or canvas)
summary: The depth a finished interface owes past working markup - a committed palette with real hierarchy, deliberate type, spatial depth, motion, and designed empty/loading/error states. Reach for it on any project with a screen or canvas so it ships a distinct, rich look instead of the flat one-accent template every build otherwise settles into, and a DIFFERENT one per project.
category: design
position: 24
structural: [has_ui]
---

# Visual craft & richness

Realise the plan's `design` and `theme` across every surface — colour hierarchy, deliberate typography, spatial depth, motion that honours `prefers-reduced-motion`, and designed empty/loading/error/first-run states. On a 3D/canvas surface, commit lighting, materials, atmosphere, and a readable HUD.

**Define tokens once, compose everywhere.** Put the palette, type scale, spacing, elevation, radius, and motion tokens as `:root` custom properties plus reusable component classes in the shell's `scope=global` stylesheet, and style every page with `var(--token)`.

## Imagery you author yourself

Default to visuals you write rather than fetch: draw illustrations, hero backdrops, icon sets, logotypes, avatars, textures, and patterns as inline SVG, CSS gradients/masks, or canvas routines so they theme, scale, and cost no runtime request. Never hotlink external image URLs or placeholder services, and never leave a bare grey box with the word "image". Use `manage_stockphotos` or `manage_image_gen` only when those tools are in your catalogue; otherwise author the artwork yourself.
