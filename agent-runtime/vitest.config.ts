import { defineConfig } from "vitest/config";

export default defineConfig({
  // Source uses NodeNext `.js` import specifiers that point at `.ts` files.
  // extensionAlias lets Vite/vitest resolve them to the TypeScript sources.
  resolve: {
    extensionAlias: {
      ".js": [".ts", ".js"],
    },
  },
  test: {
    environment: "node",
    include: ["test/**/*.test.ts"],
    testTimeout: 15000,
  },
});
