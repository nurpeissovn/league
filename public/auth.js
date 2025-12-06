// Update the entries in LEAGUES to control which passwords unlock which league.
// Keep slugs URL-safe (letters, numbers, dashes) and give each a display name.
(function () {
  const STORAGE_KEY = "league-session";
  const DEFAULT_REDIRECT = "/";
  const LEAGUES = [
    { slug: "leagueDostyq", name: "League Dostyq", password: "pass12" },
    { slug: "league-one", name: "League One", password: "pass2" },
    { slug: "league-two", name: "League Two", password: "pass3" }

  ];

  function sanitizeSlug(slug) {
    const trimmed = String(slug || "").trim().toLowerCase();
    const cleaned = trimmed.replace(/[^a-z0-9-]/g, "-").replace(/-+/g, "-");
    return cleaned || "default";
  }

  const normalizedLeagues = LEAGUES.map((l) => ({
    slug: sanitizeSlug(l.slug),
    name: String(l.name || "Unnamed League"),
    password: String(l.password || "")
  }));

  function getLeague() {
    try {
      const raw = sessionStorage.getItem(STORAGE_KEY);
      if (!raw) return null;
      const parsed = JSON.parse(raw);
      if (parsed && parsed.slug && parsed.name) return parsed;
    } catch (err) {
      console.warn("Failed to read stored league", err);
    }
    return null;
  }

  function saveLeague(league) {
    if (!league || !league.slug) return;
    sessionStorage.setItem(STORAGE_KEY, JSON.stringify(league));
  }

  function clearLeague() {
    sessionStorage.removeItem(STORAGE_KEY);
  }

  function matchPassword(password) {
    const needle = String(password || "").trim();
    if (!needle) return null;
    return normalizedLeagues.find((l) => l.password === needle) || null;
  }

  function requireLeague() {
    const league = getLeague();
    if (!league) {
      const next = encodeURIComponent(
        window.location.pathname + window.location.search
      );
      window.location.replace(`/login.html?next=${next}`);
      return null;
    }
    return league;
  }

  function leagueFetch(url, options = {}) {
    const league = getLeague();
    const opts = { ...options };
    const headers = new Headers(opts.headers || {});
    if (league?.slug) {
      headers.set("X-League-Slug", league.slug);
    }
    opts.headers = headers;
    return fetch(url, opts);
  }

  window.LeagueAuth = {
    leagues: normalizedLeagues,
    getLeague,
    saveLeague,
    clearLeague,
    matchPassword,
    requireLeague,
    leagueFetch,
    STORAGE_KEY,
    DEFAULT_REDIRECT
  };
})();
