---
name: atom-first-react-state-identity
purpose: Keep state identity stable under atom lifecycle management without relying on React memoization.
subject: Ownership and identity of Effect reactivity atoms in React.
requirement: In React code using Effect reactivity atoms, prefer atom-first
  ownership and identity rather than React useMemo to keep atom state stable.
  Have Atom.family factories return atoms directly instead of ephemeral wrapper
  objects containing atoms, so atom hooks retain the cached values without React
  memoization.
applicability: |-
  File types or languages: text/tsx.
---
```diff
-import { useMemo } from "react"
 import { useAtomValue } from "@effect/atom-react"
 import { Atom } from "effect/reactivity"

+const countAtom = Atom.family((_counterId: string) => Atom.make(0))
+
 const useCounterValue = (counterId: string) => {
-  const count = useMemo(() => Atom.make(0), [counterId])
-  return useAtomValue(count)
+  return useAtomValue(countAtom(counterId))
 }
```
