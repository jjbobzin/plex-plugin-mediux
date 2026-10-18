# plex-plugin-mediux

Automatic [Mediux](https://mediux.pro) artwork for Plex: movie and show posters,
backgrounds, logos, season posters and episode title cards, chosen by your
preferred Mediux creators.

It runs as a custom Plex metadata provider (Plex Media Server 1.43 or newer).
Plex Movie and Plex Series stay in charge of matching, titles, summaries and
watch history; this provider only supplies the artwork. Anything Mediux has no
artwork for keeps Plex's normal artwork.

**Status: early testing.** See [PLAN.md](PLAN.md) for the design and what has
been tested so far.

## Install

1. **Run the container.** Use [docker-compose.yml](docker-compose.yml) as is,
   for example as a new stack in Dockhand or Portainer. It publishes port
   `38600` and keeps its settings in `/mnt/cache/appdata/plex-plugin-mediux`
   (change the left side of the volume to your own appdata location).
2. **Open the settings page** at `http://<server-ip>:38600` and paste your
   Mediux API token (from your account settings on mediux.pro). Choose your
   creators and artwork options, then save. Nothing is configured through
   environment variables or files.
3. **Add the providers in Plex:** Settings → Metadata Agents → Add Provider,
   once with `http://<server-ip>:38600/movies` and once with
   `http://<server-ip>:38600/tv`.
4. **Create an agent for each:** pick **Plex Movie** (or **Plex Series**) as
   the Primary, add **Mediux Artwork**, and drag it to the top so its artwork
   wins. The Primary cannot be changed after the agent is created.
5. **Use the agent** for a library (library → Edit → Advanced → Agent) and
   refresh. Try it on a small test library first.

Plex reads the movie or show ID from TMDB/TVDB tags in file names, such as
`{tmdb-291870}` from Radarr or Sonarr naming. Without a tag it falls back to
an exact title and year match.

**Opening the settings page from outside your home network:** put it behind a
reverse proxy that asks for a login (Pangolin, Authelia, Cloudflare Access and
so on). The page holds your Mediux key, so don't expose it without one. Plex
keeps using the local `http://<server-ip>:38600/...` addresses either way.

## What else it does

- **AURA picks.** If you choose sets in [AURA](https://github.com/mediux-team/AURA),
  share AURA's data folder with the container (the commented line in the
  compose file) and the provider uses your saved sets ahead of creator
  priority, on every Plex server. It only reads AURA's database.
- **Watch for new artwork.** Plex only asks for artwork when a title is added
  or refreshed. The provider re-checks Mediux on a schedule for titles Plex has
  asked about. When something new appears, such as a title card uploaded after
  the episode aired, it asks your Plex servers to refresh that title and can
  post to a Discord channel. Add Plex servers and a Discord webhook on the
  **Watch & notify** tab.
- **Try it** shows what your settings pick for any title, and **Log** lists what
  Plex asked for and what happened.

Posters you uploaded or locked in Plex, for example through AURA or Kometa, are
never replaced.

## Credits

Artwork selection is adapted from
[Pukabyte's Mediux plugin for Silo](https://github.com/Pukabyte/silo-plugin-mediux).
Not affiliated with Mediux or Plex.

## License

[AGPL-3.0-or-later](LICENSE).
