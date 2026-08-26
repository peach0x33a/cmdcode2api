import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The UI is served by the Go gateway under /ui (see internal/app/ui.go), not
// site root, so every asset URL Vite emits must be prefixed accordingly.
// Build output lands in internal/app/webui/dist — a sibling of this source
// directory — so //go:embed webui/dist in ui.go can pick it up as a
// self-contained static tree.
export default defineConfig({
  plugins: [react()],
  base: "/ui/",
  build: {
    outDir: "../webui/dist",
    emptyOutDir: true,
  },
});
