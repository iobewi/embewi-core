# Configuration Sphinx — documentation Embewi Core.
# Markdown consommé directement via MyST-Parser (aucune réécriture des .md).

project = "Embewi Core"
author = "Embewi"
copyright = "2026, Embewi"
language = "fr"

extensions = ["myst_parser", "sphinx_rtd_theme", "sphinxcontrib.mermaid"]

source_suffix = {".md": "markdown"}
myst_enable_extensions = ["colon_fence", "deflist"]
myst_heading_anchors = 3

exclude_patterns = ["_build", "requirements.txt", "conf.py", "Thumbs.db", ".DS_Store"]

# Supprime le warning de coloration syntaxique sur les blocs JSON tronqués (ex. "...").
suppress_warnings = ["misc.highlighting_failure"]

html_theme = "sphinx_rtd_theme"
html_title = "Embewi Core — Documentation"
html_theme_options = {
    "navigation_depth": 3,
    "collapse_navigation": False,
    "style_external_links": True,
}

html_static_path = ["_static"]
html_logo = "_static/logo.png"
html_favicon = "_static/favicon.ico"
