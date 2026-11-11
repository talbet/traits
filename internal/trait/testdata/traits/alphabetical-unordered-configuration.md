---
name: alphabetical-unordered-configuration
purpose: Make unordered configuration easy to scan and reduce incidental diff churn without changing execution semantics.
subject: Configuration lists and fields whose ordering has no semantic significance.
requirement: 'Prefer alphabetical ordering for miscellaneous configuration lists
  and fields when their order has no semantic significance: projects:
  ["oxlints/vite.config.ts", "traits.sh/vite.config.ts", "traits/vite.config.ts"].
  Preserve intentional dependency, execution, precedence, and user-facing order
  rather than sorting those blindly.'
applicability: |-
  Applies when editing configuration lists or fields whose ordering has no semantic significance.
  File types or languages: text/typescript, text/javascript, application/json, application/yaml, application/toml.
---
```diff
-projects: ["traits/vite.config.ts", "oxlints/vite.config.ts"]
+projects: ["oxlints/vite.config.ts", "traits/vite.config.ts"]
```
