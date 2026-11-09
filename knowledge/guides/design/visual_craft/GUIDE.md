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

A working page is not a finished one. Told only to build the feature, a model settles
into the one safe look it reaches for every time (a flat surface, a lone accent on grey,
one system font, no depth, no motion, undesigned blank states), which is why distinct
projects come out looking like the same template.

**Commit to ONE specific look the project earns, realise it on every surface, and make it
DIFFERENT across projects so two never share a look by default.** A strong model already
knows how to write the CSS, the keyframes, and the shader; the value here is forcing the
commitment, the depth, and the variety, not teaching the technique. Carry the direction
`design` names rather than re-deriving it: sharpen a set one, invent an open one. Colours and
type follow the plan's `theme` (the audience and mood), never a default kit.

Make the chosen look real, not gestured at: colour with hierarchy rather than one accent on
grey; type chosen on purpose; deliberate spacing, elevation, and layering so the screen reads
in order; motion that serves the look and always honours `prefers-reduced-motion`; and every
state built as a moment in the look (empty, loading, error, success, first-run), never a bare
string. A look most often falls flat not on colour but on depth and signature: a fine palette
on square, shadowless cards with no recurring move still reads as the template - commit the
elevation and the one signature move too.

**Define the look once, compose everywhere.** So the one committed look holds across every
screen instead of drifting per page, realise it as a system in the global (`scope=global`)
stylesheet the shell task set up: the palette (colour roles with a tonal scale, not a lone
accent), the type scale, spacing, elevation, radius, and motion as `:root` custom properties,
plus the reusable component classes (buttons, inputs, cards, fields, nav). Every page and
component then styles FROM those tokens and classes - `var(--token)`, not a fresh hex or px
value on each screen - so the base is set in one place and no page re-invents it. Choose the
token VALUES to fit THIS project's direction; never carry a fixed set over, or every build
lands on the same look.

**A 3D or canvas surface owes the same in its own terms**: committed lighting, materials, and
atmosphere that fit the world, plus a readable HUD, not a lit primitive on an empty plane. When
the art direction is open, pick one and commit, and vary it across projects.

## Imagery you author yourself

Default to visuals you write rather than fetch. Draw it as inline SVG, a CSS
gradient or mask, or a canvas routine: an illustration, a hero backdrop, an icon set, a
logotype, an avatar, a texture, a pattern. These are code, so they scale, theme, and
animate, and they cost no request at runtime. Hotlinking an external image URL or an
`https://placeholder` service ships a page that breaks when that host does; a bare grey box
with the word "image" is not a design.

Photography or generated imagery comes only from the image tools (manage_stockphotos,
manage_image_gen) when your catalogue lists them; if it lists none, that capability is
not configured here: author the artwork and move on.
