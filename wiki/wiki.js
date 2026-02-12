const NAV = [
  {
    section: "Start Here",
    pages: [
      ["Home", "Home"],
      ["Get Started", "Get-Started"],
      ["Overview", "Overview"],
      ["Architecture", "Architecture"],
    ],
  },
  {
    section: "Core Concepts",
    pages: [
      ["Domain Model", "Domain-Model"],
      ["Queueing Model", "Queueing-Model"],
      ["Storage (KVRocks)", "Storage-KVRocks"],
      ["Consistency", "Consistency"],
      ["Retry and Backoff", "Retry-and-Backoff"],
      ["Webhooks", "Webhooks"],
      ["Security", "Security"],
    ],
  },
  {
    section: "Interfaces",
    pages: [
      ["HTTP API", "HTTP-API"],
      ["CLI", "CLI"],
      ["Configuration", "Configuration"],
    ],
  },
  {
    section: "Operations",
    pages: [
      ["Operations", "Operations"],
      ["Migration", "Migration"],
      ["Sharding", "Sharding"],
      ["Examples", "Examples"],
    ],
  },
  {
    section: "Use Cases",
    pages: [
      ["Use Cases", "Use-Cases"],
      ["Use Case: Enqueue, Claim, Complete", "Use-Cases-Enqueue-Claim-Complete"],
      ["Use Case: Long-Poll Claim", "Use-Cases-Long-Poll-Claim"],
      ["Use Case: NACK + Retry Backoff", "Use-Cases-NACK-Retry-Backoff"],
      ["Use Case: Lease Expiry Repair", "Use-Cases-Lease-Expiry-Repair"],
      ["Use Case: Worker Availability Webhook", "Use-Cases-Worker-Availability-Webhook"],
      ["Use Case: Result Callbacks", "Use-Cases-Result-Callbacks"],
      ["Use Case: Admin Cleanup", "Use-Cases-Admin-Cleanup"],
    ],
  },
];

const DEFAULT_PAGE = "Home";
const navEl = document.getElementById("nav");
const contentEl = document.getElementById("content");
const searchEl = document.getElementById("search");
const currentPageEl = document.getElementById("current-page");
const ALL_PAGE_SLUGS = NAV.flatMap((group) => group.pages.map((p) => p[1]));

let mermaidConfigured = false;

marked.setOptions({
  gfm: true,
  breaks: false,
  mangle: false,
  headerIds: true,
});

function getPageFromUrl() {
  const page = new URLSearchParams(window.location.search).get("page");
  if (page && ALL_PAGE_SLUGS.includes(page)) {
    return page;
  }
  return DEFAULT_PAGE;
}

function pageLabel(slug) {
  for (const group of NAV) {
    const found = group.pages.find((entry) => entry[1] === slug);
    if (found) {
      return found[0];
    }
  }
  return slug.replaceAll("-", " ");
}

function escapeHtml(value) {
  return value
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;")
    .replaceAll("'", "&#39;");
}

function rewriteInternalLinks() {
  const anchors = contentEl.querySelectorAll("a[href]");
  for (const anchor of anchors) {
    const href = anchor.getAttribute("href") || "";
    if (
      !href ||
      href.startsWith("http://") ||
      href.startsWith("https://") ||
      href.startsWith("mailto:") ||
      href.startsWith("#") ||
      href.startsWith("/")
    ) {
      continue;
    }

    const [rawPath, rawHash] = href.split("#");
    const clean = (rawPath || "")
      .replace(/^\.\//, "")
      .replace(/\.md$/, "")
      .replace(/\/$/, "");

    if (!ALL_PAGE_SLUGS.includes(clean)) {
      continue;
    }

    const hash = rawHash ? `#${rawHash}` : "";
    anchor.setAttribute("href", `?page=${encodeURIComponent(clean)}${hash}`);
  }
}

function renderNavigation(activePage, filterText = "") {
  const normalizedFilter = filterText.trim().toLowerCase();
  navEl.innerHTML = "";

  for (const group of NAV) {
    const filteredPages = group.pages.filter(([label, slug]) => {
      if (!normalizedFilter) {
        return true;
      }
      return label.toLowerCase().includes(normalizedFilter) || slug.toLowerCase().includes(normalizedFilter);
    });

    if (!filteredPages.length) {
      continue;
    }

    const groupWrap = document.createElement("section");
    groupWrap.className = "nav-group";

    const title = document.createElement("h3");
    title.className = "nav-title";
    title.textContent = group.section;
    groupWrap.appendChild(title);

    const list = document.createElement("ul");
    list.className = "nav-list";

    for (const [label, slug] of filteredPages) {
      const li = document.createElement("li");
      const link = document.createElement("a");
      link.href = `?page=${encodeURIComponent(slug)}`;
      link.textContent = label;
      link.className = `nav-link${slug === activePage ? " active" : ""}`;
      li.appendChild(link);
      list.appendChild(li);
    }

    groupWrap.appendChild(list);
    navEl.appendChild(groupWrap);
  }
}

function upgradeMermaidBlocks() {
  const mermaidCodeBlocks = contentEl.querySelectorAll("pre code.language-mermaid, code.language-mermaid");

  for (const codeBlock of mermaidCodeBlocks) {
    const diagramText = (codeBlock.textContent || "").trim();
    if (!diagramText) {
      continue;
    }

    const wrapper = document.createElement("div");
    wrapper.className = "mermaid";
    wrapper.textContent = diagramText;

    const pre = codeBlock.closest("pre");
    if (pre) {
      pre.replaceWith(wrapper);
    } else {
      codeBlock.replaceWith(wrapper);
    }
  }
}

function ensureMermaidConfigured() {
  if (mermaidConfigured || typeof mermaid === "undefined") {
    return;
  }
  mermaid.initialize({
    startOnLoad: false,
    securityLevel: "loose",
    theme: "dark",
    fontFamily: "Inter, sans-serif",
    themeVariables: {
      primaryColor: "#0f172a",
      primaryTextColor: "#f8fafc",
      primaryBorderColor: "#06b6d4",
      lineColor: "#06b6d4",
      secondaryColor: "#020617",
      tertiaryColor: "#020617",
      actorBorder: "#06b6d4",
      actorBkg: "#0f172a",
      actorTextColor: "#f8fafc",
      signalColor: "#8b5cf6",
      labelBoxBkgColor: "#0f172a",
      labelBoxBorderColor: "#06b6d4",
      labelTextColor: "#f8fafc",
      noteBkgColor: "#111c33",
      noteBorderColor: "#06b6d4",
      noteTextColor: "#e2e8f0",
      background: "#020617",
    },
  });
  mermaidConfigured = true;
}

async function renderMermaid() {
  const blocks = contentEl.querySelectorAll(".mermaid");
  if (!blocks.length) {
    return;
  }

  ensureMermaidConfigured();

  if (typeof mermaid === "undefined") {
    return;
  }

  try {
    await mermaid.run({ nodes: blocks });
  } catch (error) {
    console.error("Mermaid render error:", error);
    for (const block of blocks) {
      if (block.querySelector("svg")) {
        continue;
      }
      const fallback = document.createElement("pre");
      const fallbackCode = document.createElement("code");
      fallbackCode.textContent = block.textContent || "";
      fallback.appendChild(fallbackCode);
      block.replaceWith(fallback);
    }
  }
}

async function loadPage(pageSlug) {
  currentPageEl.textContent = pageLabel(pageSlug);
  document.title = `${pageLabel(pageSlug)} | codeQ Docs`;
  renderNavigation(pageSlug, searchEl.value);

  try {
    const response = await fetch(`${pageSlug}.md`, { cache: "no-store" });
    if (!response.ok) {
      throw new Error(`Unable to load page ${pageSlug} (status ${response.status})`);
    }

    const markdown = await response.text();
    contentEl.innerHTML = marked.parse(markdown);
    upgradeMermaidBlocks();
    rewriteInternalLinks();
    await renderMermaid();
  } catch (error) {
    contentEl.innerHTML = `
      <div class="error-card">
        <strong>Could not load this page.</strong><br>
        ${escapeHtml(error.message)}
      </div>
    `;
  }
}

window.addEventListener("popstate", () => {
  loadPage(getPageFromUrl());
});

document.addEventListener("click", (event) => {
  const target = event.target.closest("a[href]");
  if (!target) {
    return;
  }

  const href = target.getAttribute("href") || "";
  if (!href.startsWith("?page=")) {
    return;
  }

  event.preventDefault();
  const url = new URL(href, window.location.href);
  const page = url.searchParams.get("page");
  if (!page || !ALL_PAGE_SLUGS.includes(page)) {
    return;
  }

  history.pushState({}, "", `?page=${encodeURIComponent(page)}`);
  loadPage(page);
});

searchEl.addEventListener("input", () => {
  renderNavigation(getPageFromUrl(), searchEl.value);
});

loadPage(getPageFromUrl());
