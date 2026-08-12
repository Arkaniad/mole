# Contributor License Agreement

> **⚠ DRAFT — NOT LEGAL TEXT. DO NOT PUBLISH AS-IS.**
>
> This is a working outline for a lawyer to turn into an agreement, not an
> agreement. It has not been reviewed by anyone qualified. Publishing it in this
> state would be worse than having no CLA: contributors would sign something
> whose effect nobody has verified, which is a defect in exactly the direction
> this project cares about.
>
> **Before launch, replace this file entirely with reviewed text.**
>
> The usual base is the Apache Software Foundation's ICLA and CCLA
> (<https://www.apache.org/licenses/#clas>), adapted to name an individual or
> company instead of the Foundation. They are widely understood, and a
> contributor who has signed one before will recognise the shape.

## Open questions for the lawyer

**1. Who is the counterparty?** The agreement must name the entity receiving the
grant. Today `NOTICE` says `Lajos Deme` (an individual).

If a company is to be formed for the hosted product, the CLA should name that
company from the first signature. Otherwise every contribution is granted to a
person, and moving those rights into the company later means either an
assignment from the individual (straightforward) or re-collecting signatures
(not). **This is the question worth resolving before the repository goes
public**, because it is cheap now and expensive after the tenth contributor.

**2. Licence grant or copyright assignment?** The intent is a *grant*, not an
assignment: the contributor keeps their copyright and may reuse their own work
anywhere. The grant needs to be broad enough to permit releasing future versions
under different terms — including a source-available licence — which is the
entire purpose (see `CONTRIBUTING.md`).

**3. Patent grant.** Apache-2.0 §3 already carries one for the inbound
contribution. Confirm the CLA does not narrow it and that the defensive
termination clause behaves sensibly for a single-entity project.

**4. Corporate contributions.** A CCLA is needed for contributors working on
company time, since their employer may own the copyright. Without one, a
company's legal team can invalidate a contribution after the fact.

**5. The commitment in `CONTRIBUTING.md`.** That document promises publicly that
every released version stays Apache-2.0 permanently, and that a relicence could
only apply to future versions. Decide with counsel whether that promise should
be binding text inside the CLA — a contributor-facing guarantee is worth more if
it is enforceable, and the promise costs nothing to keep since it describes what
Apache-2.0's irrevocability already guarantees for published releases.

## Intended effect, in plain terms

For a contributor, in one sentence: *you keep your copyright, your contribution
ships under Apache-2.0 like everything else, and the maintainer may also release
future versions of the project under other terms.*

## Mechanics once the text exists

- **cla-assistant.io** — GitHub app, comment-to-sign, signatures stored in a
  gist. The standard choice for a single maintainer; no infrastructure.
- Two documents: individual (ICLA) and corporate (CCLA).
- Keep the DCO sign-off requirement alongside it (`git commit -s`). They answer
  different questions — DCO is "do you have the right to submit this", the CLA is
  "what may the project do with it" — and having both is normal.
