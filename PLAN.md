# Mediux for Plex: build plan

A small service that gives Plex Mediux artwork automatically: movie and show
posters, backgrounds, logos, season posters and episode title cards. It works
like Pukabyte's Mediux plugin for Silo, using Plex's new custom metadata
providers. It replaces the manual "find a set, apply it" step in AURA.

Status: proven feasible on 2026-10-17 (see "What the test proved"). Nothing
real is built yet.

## What the test proved

We ran a throwaway provider (`test-provider/`) against the real server with
one movie (Dirty Grandpa) and one show (1923):

- **Our artwork wins** when our provider sits above Plex Movie / Plex Series
  in the agent. The poster, background, season posters and each episode's title
  card all came from us, on the right episodes.
- **Plex stays in charge of matching.** Plex Movie / Plex Series remain the
  Primary, so titles, summaries, ratings and watch history are untouched
  (watched checkmarks carried over).
- **We must send artwork only.** Plex fills each field from the first provider
  that has it. When the test sent a date, it overwrote the real one.
- **Plex sends no TMDB/IMDb/TVDB ID**, only the title, year and file name. For
  TV it sends just `Season 02/<file name>`, not the show's folder.
- **Changed artwork needs a new address**, or Plex keeps the old image. Mediux
  addresses already include a version number that changes with the artwork.
- **The image picker shows only the top provider's images.** With ours on
  top, Plex's own poster choices disappear from Edit → Poster.
- **Plex asks for show episodes per season** and passes an episode order
  (`tmdbAiring`), which matches how Mediux numbers title cards.
- **An agent's Primary is fixed when the agent is created.** Reordering only
  changes field priority.

## How it will work

1. Plex scans or refreshes an item and asks our provider about it.
2. The provider works out which movie or show it is (see "Finding the item").
3. It asks Mediux for every artwork set for that item, using your Mediux token.
4. It picks the artwork by your rules (creator priority, exclusions,
   consistency settings) and hands Plex only the image addresses.
5. Plex downloads the images straight from Mediux. Your token is never in an
   image address.

It runs as one Docker container on Unraid, deployed with Dockhand. Plex reaches
it inside your network at `http://<server-ip>:32600`, so nothing is exposed to
the internet.

- Movies provider: `http://<server-ip>:32600/movies`
- TV provider: `http://<server-ip>:32600/tv`
- Settings page (in a browser): `http://<server-ip>:32600/`

## Finding the item

Plex doesn't tell us the ID, so the provider tries in this order:

| Kind | First choice | Fallback |
|---|---|---|
| Movies | `{tmdb-12345}` in the file name (your Radarr naming already has it) | Search by title and year |
| TV | `{tvdb-…}` or `{tmdb-…}` in an episode file name, if present | Search by title (TVmaze → TVDB ID → Mediux), which found "1923" correctly |

Mediux identifies movies and shows by TMDB ID and can look shows up by TVDB ID,
so no TMDB key is needed for finding items. A free TMDB key is only needed for
the optional "also show regular posters in the picker" feature.

## Settings (yours to review and own)

These mirror the Silo plugin's settings page from your screenshots. Defaults
are proposals; tell me what you'd change.

| Setting | Proposed default | What it does |
|---|---|---|
| Mediux API token | (required) | Your key to Mediux. Saved privately, never shown again. |
| Creator priority | willtong93, r3draid3r04, Pejamas, fwlolx, tallinex | Top creator wins when several have artwork. |
| Excluded creators | MrMagixx, CmdrRiker | Never used. |
| Only use prioritized creators | Off | When on, creators outside your list are ignored. |
| Season poster consistency | Creator priority first; most artwork per creator | How season posters are kept from one set. |
| Title card consistency | Creator priority first; most artwork per creator | Same, for episode title cards. |
| Missing artwork in selected set | Keep existing; no other Mediux set | What to do when the chosen set lacks a season or episode. |
| Image quality | Optimized JPEG | Smaller files; "Original" keeps full quality. Logos stay original. |
| Cache hours | 24 | How long Mediux answers are reused. 0 turns it off. |
| Also list regular posters in the picker | Off (needs TMDB key) | Puts TMDB posters below Mediux ones in Edit → Poster. |

Plex may support settings inside Plex itself (it asked our test provider for
"preferences"), but that isn't documented yet, so settings live on the
provider's own page.

## User stories (for you to review)

- As the Plex owner, when Radarr or Sonarr adds a movie or show, it gets Mediux
  artwork from my preferred creators without me doing anything.
- As the Plex owner, a show's season posters and title cards come from one
  matching set whenever one exists, so the show looks consistent.
- As the Plex owner, when Mediux has no artwork for something, it keeps normal
  Plex artwork, never a blank.
- As the Plex owner, I can still pick a different Mediux poster by hand in
  Edit → Poster, and my pick isn't overwritten on the next refresh.
- As the Plex owner, I change creator order on the settings page, refresh an
  item, and the new choice shows up, without restarting anything.
- As a viewer, nothing changes except the artwork: titles, summaries and my
  watch history stay the same.

## Build phases

Each phase ends with a test you run on the two test libraries
("zMediux Movies" and "zMediux TV"), which stay up until the project is done.

1. **Setup.** GitHub repo, project board (standard lanes), container image on
   GitHub's registry, compose file for Dockhand (Dockhand can deploy straight
   from the repo and redeploy on updates). You add the providers and two test
   agents in Plex: Plex Movie / Plex Series as Primary, ours dragged to the top.
2. **Movies.** Token, ID from file name with title fallback, poster,
   background and logo by creator priority and exclusions, all Mediux options
   in the picker. Test: a handful of movies, some with no Mediux art.
3. **TV.** Show poster, background and logo, season posters, title cards,
   consistency and gap settings. Test: 3 to 4 shows, including one with
   specials and one that isn't on Mediux.
4. **Settings page.** Creator search, ordering and exclusions, plus the
   artwork options above. Test: change the order, refresh, and confirm the
   artwork follows.
5. **Rollout.** Switch your real Movies and TV libraries to the new agents,
   following a written checklist: what to check before and after, and how to
   switch back if something looks wrong.

## Still to verify (built into the phase tests)

- **No Mediux art → normal Plex art.** Expected but not yet confirmed.
- **Plex can download from Mediux directly.** Pukabyte notes some clients get
  blocked. If Plex is one of them, the provider relays the images itself.
- **Switching a real library to the new agent.** The Primary stays the same,
  so matches and watch history should hold. We confirm on the test libraries
  first, and the rollout checklist covers switching back.
- **Posters set with AURA.** Plex usually locks hand-picked posters, so those
  likely stay as they are. Phase 2 checks this on one AURA-styled movie.
- **Settings inside Plex.** An optional later step if Plex documents it.

## Decisions for you

- **License.** Pukabyte's plugin is AGPL. If ours is AGPL too, I can adapt
  their tested set-selection logic directly (credited), which is faster and
  less risky. A different license means rewriting it from scratch.
- **Public or private repo.** Public lets others use it and lets Dockhand pull
  without a login; private keeps it to you.
