#!/usr/bin/env python3
"""
nordnet_skv.py - Build SKV 2198 tables ("Genomsnittligt omkostnadsbelopp") and K4 sales
summaries for shares held and sold through Nordnet.

Uses only the Python standard library.

Input files (looked up in the given directory):
  Nordnet*.csv           - Nordnet "transaktioner och notor" exports (one or more, UTF-16,
                           tab separated). Every security in the exports is handled
                           separately; rows sharing a name or ISIN are the same security
                           (so an ISIN change at a split keeps the history together).
  Omkostnadsbelopp*.csv  - optional: cost basis for shares deposited into Nordnet
                           (INLÄGG VP, e.g. transfers from another custodian) and for
                           holdings before the export starts. Semicolon separated:
                             Värdepapper;Datum;Antal;Omkostnadsbelopp;Händelse;Kommentar
                           Värdepapper is the name or ISIN as in the Nordnet export, Datum
                           YYYY-MM-DD, Antal in shares valid on that date, Omkostnadsbelopp
                           in kr (total for the row), Händelse Köp/Förm/Övr (default Köp).
                           Several rows may share a date. Rows dated before a security's
                           first Nordnet transaction are its opening holding. If the file
                           is missing, a template (mall_omkostnadsbelopp.csv) listing every
                           deposit is written to fill in.

Outputs:
  skv2198.csv    - SKV 2198 columns 1-8 (+ comment), per security, chronological
  k4.csv         - one row per sale and per-year totals for K4 section A
  k4_innehav.csv - holdings log: one row per event with K4 values on sale rows and the
                   holding after each event

Tax treatment used:
  * KÖPT -> "Köp", cost incl. fees.
  * SÅLT -> "Förs": utnyttjat omkostnadsbelopp = antal x genomsnitt.
  * SPLIT UTTAG/INLÄGG VP -> "S": quantity x ratio (from the transaction text, checked
    against the quantities), total cost unchanged.
  * TECKNING INLÄGG VP (shares subscribed for in a new issue) -> "Köp" at the subscription
    price; the rights used (TECKNING UT RÄTTER) are removed, and if they had a cost a
    warning says to add it to the new shares. TECKNING LIKVID (the payment) is ignored.
  * INLÄGG VP and other deposits (OMVANDLING, BYTE, EMISSION, ...) -> Köp/Förm/Övr with the cost
    from the cost file, else Nordnet's inköpsvärde, else 0 kr with a warning.
  * TILLDELNING INLÄGG of subscription/unit rights (teckningsrätter, uniträtter) -> "Övr"
    with omkostnadsbelopp 0 kr, unless the cost file says otherwise.
  * INLÄGG VP from an ISK (text mentions ISK) -> cost = market value on the day: the price
    in the row if Nordnet gives one, otherwise from the cost file (warning).
  * UTTAG VP matched by an INLÄGG VP of the same quantity in another of your Nordnet
    accounts within a week, or in the same account on the same day (Nordnet re-booking a
    security, e.g. EM UTTAG/EM INLÄGG): a move, not in the table.
  * UTTAG VP RESULTAT (e.g. a move into an ISK) -> "Förs" at market value (antal x kurs);
    the result is checked against Nordnet's "Resultat".
  * Every sale's result is checked against Nordnet's "Resultat" when Nordnet has one
    (non-zero); a difference of more than 1 kr gives a warning.
  * INLÖSEN UTTAG VP (redemption, expired rights) -> "Förs" for the amount in the row; if
    both that amount and the omkostnadsbelopp are 0 (worthless rights received for free)
    the shares are just removed and nothing goes on K4.
  * Other withdrawals (OMVANDLING UTTAG, UTTAG VP to another custodian, ...) -> "Övr" that
    removes the shares at the average cost, with a warning.
  * Dividends, taxes and fees do not change the omkostnadsbelopp.
ISK and kapitalförsäkring are not declared on K4: leave those accounts out with --depa.
"""

from __future__ import annotations

import argparse
import csv
import datetime as dt
import glob
import os
import re
from dataclasses import dataclass, field

TOL = 0.001
COST_PATTERN = "Omkostnadsbelopp*.csv"
TEMPLATE = "mall_omkostnadsbelopp.csv"
COST_KINDS = ("Köp", "Förm", "Övr")
TRANSFER_DAYS = 7      # max days between UTTAG VP and INLÄGG VP of a move between accounts
# No effect on holdings or cost (cash legs such as TECKNING LIKVID included).
IGNORED = ("UTDELNING", "SKATT", "RÄNTA", "AVGIFT", "KUPONG", "LIKVID")
# Withdrawals whose cost goes on to another security (swap, conversion, subscription).
CONVERSIONS = ("OMVANDLING", "BYTE", "TECKNING")

warnings: list[str] = []


def warn(msg: str) -> None:
    warnings.append(msg)


# --------------------------------------------------------------------------- parsing


def num(s: str | None) -> float | None:
    s = (s or "").strip().replace(" ", "").replace(" ", "")
    if s in ("", "-"):
        return None
    try:
        return float(s.replace(",", "."))
    except ValueError:
        return None


def decode(raw: bytes) -> str:
    if raw[:2] in (b"\xff\xfe", b"\xfe\xff"):
        return raw.decode("utf-16")
    for enc in ("utf-8-sig", "cp1252"):
        try:
            return raw.decode(enc)
        except UnicodeDecodeError:
            continue
    return raw.decode("latin-1")


def read_table(path: str) -> list[dict[str, str]]:
    """Read a tab/semicolon/comma separated file into dicts. A repeated header name
    (Nordnet has several "Valuta" columns) is renamed "<name> (<previous column>)",
    e.g. "Valuta (Belopp)"."""
    with open(path, "rb") as f:
        lines = decode(f.read()).splitlines()
    if not lines:
        return []
    first = lines[0]
    delim = "\t" if "\t" in first else ";" if ";" in first else ","
    reader = csv.reader(lines, delimiter=delim)
    header: list[str] = []
    for h in next(reader):
        h = h.strip()
        header.append(f"{h} ({header[-1]})" if h in header and header else h)
    rows = []
    for rec in reader:
        if any(x.strip() for x in rec):
            rows.append({name: val.strip() for name, val in zip(header, rec)})
    return rows


def find(directory: str, pattern: str) -> list[str]:
    return sorted(glob.glob(os.path.join(directory, pattern)))


# --------------------------------------------------------------------------- model

# Order of events on the same date.
ORDER = {"S": 0, "Köp": 1, "Förm": 2, "Övr": 2, "Ut": 3, "Inl": 3, "Förs": 3}


@dataclass
class Event:
    date: dt.date
    # Köp, Förm, Övr, S, Förs, Ut (withdrawal at average cost, shown as Övr),
    # Inl (redemption: a sale, or just a removal if there is neither payment nor cost)
    kind: str
    qty: float = 0.0   # shares in (+); for Förs/Ut/Inl the number of shares out (positive)
    cost: float = 0.0  # added omkostnadsbelopp (Köp/Förm/Övr)
    proceeds: float = 0.0  # Förs/Inl: net sale amount after fees
    ratio: float = 1.0     # S: new shares per old share
    note: str = ""
    ref: str = ""
    nordnet_result: float | None = None  # Förs: Nordnet's "Resultat", for a cross-check
    carry: str = ""    # Ut: CONVERSIONS keyword when the cost goes on to another security

    @property
    def order(self) -> int:
        return ORDER[self.kind]


@dataclass
class Deposit:
    date: dt.date
    qty: float
    text: str
    depa: str
    kind: str                 # Nordnet transaktionstyp
    nordnet_cost: float | None
    price: float = 0.0        # "Kurs" in the row
    right: bool = False       # allotted subscription/unit right
    internal: bool = False    # move between own accounts


@dataclass
class Security:
    name: str
    names: set[str] = field(default_factory=set)
    isins: set[str] = field(default_factory=set)
    events: list[Event] = field(default_factory=list)
    deposits: list[Deposit] = field(default_factory=list)
    start_date: dt.date | None = None
    opening: float = 0.0          # holdings just before start_date (units of that date)
    closing: float = 0.0
    closing_date: dt.date | None = None
    eod: dict[dt.date, float] = field(default_factory=dict)  # holdings at end of trade date
    in_transit: list[tuple[dt.date, dt.date]] = field(default_factory=list)

    def matches(self, key: str) -> bool:
        k = key.strip().upper()
        return k in self.isins or k in {n.upper() for n in self.names}


@dataclass
class CostRow:
    security: str
    date: dt.date
    qty: float
    cost: float | None
    kind: str
    note: str


# --------------------------------------------------------------------------- Nordnet


def tdate(r) -> dt.date:
    return dt.date.fromisoformat(r.get("Affärsdag") or r["Bokföringsdag"])


def ttype(r) -> str:
    return r.get("Transaktionstyp", "").strip().upper()


def is_ignored(t: str) -> bool:
    return any(x in t for x in IGNORED)


def is_out(t: str) -> bool:
    """A withdrawal: "... UTTAG ..." or e.g. "TECKNING UT RÄTTER"."""
    return "UTTAG" in t or bool(re.search(r"\bUT\b", t))


def conversion(t: str) -> str:
    return next((c for c in CONVERSIONS if c in t), "")


def delta(t: str, q: float) -> float | None:
    """Holding change of a row; None for an unknown type."""
    if t == "KÖPT":
        return q
    if t == "SÅLT":
        return -q
    if is_ignored(t):
        return 0.0
    if "INLÄGG" in t:
        return q
    if is_out(t):
        return -q
    return None


RIGHT_NAME = re.compile(r"\b(B?TR|UR|TECKNINGSR\S*|UNITR\S*)$")


def is_right(t: str, name: str, text: str) -> bool:
    """An allotted subscription or unit right (teckningsrätt/uniträtt)."""
    u = text.upper()
    return "TILLDELNING" in t and (bool(RIGHT_NAME.search(name.upper().strip()))
                                   or "TECKNINGSR" in u or "UNITR" in u or "RÄTTER" in u)


def nordnet_result(r) -> float | None:
    """Nordnet's realised result for the row; 0 or empty means Nordnet does not know the
    cost (e.g. shares deposited from elsewhere), so there is nothing to check against."""
    res = num(r.get("Resultat"))
    return res if res else None


def split_ratio(text: str) -> float | None:
    """'Split ABC B, 4:1' -> 4.0 (new shares per old share)."""
    m = re.search(r"(\d+(?:[.,]\d+)?)\s*:\s*(\d+(?:[.,]\d+)?)", text)
    if not m:
        return None
    a, b = (float(x.replace(",", ".")) for x in m.groups())
    return a / b if a and b else None


def load_nordnet(paths: list[str], depas: list[str] | None,
                 only: list[str] | None) -> tuple[list[Security], set[str]]:
    """Return the securities in the exports (only those matching `only`, a list of names
    or ISINs, if given) and the upper-cased names and ISINs of all securities."""
    seen: set[str] = set()
    rows = []
    for p in paths:
        for r in read_table(p):
            if not r.get("Värdepapper") or r.get("Makuleringsdatum"):
                continue
            if depas and r.get("Depå", "") not in depas:
                continue
            key = r.get("Id") or "|".join(r.values())
            if key in seen:
                continue
            seen.add(key)
            rows.append(r)

    # Group rows into securities: rows sharing a name or an ISIN belong together.
    parent: dict[str, str] = {}

    def root(x: str) -> str:
        while parent.setdefault(x, x) != x:
            parent[x] = parent[parent[x]]
            x = parent[x]
        return x

    for r in rows:
        a = "N:" + r["Värdepapper"].upper()
        root(a)
        if r.get("ISIN"):
            parent[root("I:" + r["ISIN"].upper())] = root(a)
    groups: dict[str, list[dict]] = {}
    for r in rows:
        groups.setdefault(root("N:" + r["Värdepapper"].upper()), []).append(r)

    def rid(r):
        try:
            return int(r.get("Id") or 0)
        except ValueError:
            return 0

    out = []
    known = {r["Värdepapper"].upper() for r in rows} | {r["ISIN"].upper() for r in rows if r.get("ISIN")}
    wanted = {k.strip().upper() for k in only or []}
    for k in sorted(wanted - known):
        warn(f"--security {k}: finns inte i Nordnet-exporten.")
    for grp in groups.values():
        keys = {r["Värdepapper"].upper() for r in grp} | {r["ISIN"].upper() for r in grp if r.get("ISIN")}
        if wanted and not keys & wanted:
            continue
        # Same day: SPLIT UTTAG before SPLIT INLÄGG, otherwise by Id.
        grp.sort(key=lambda r: (tdate(r), 0 if ttype(r) == "SPLIT UTTAG VP" else 1, rid(r)))
        out.append(process_security(grp))
    return sorted(out, key=lambda s: s.name.upper()), known


def process_security(rows: list[dict]) -> Security:
    s = Security(name=rows[-1]["Värdepapper"])
    s.start_date = tdate(rows[0])
    hold: dict[str, float] = {}           # holdings per Nordnet account (Depå)
    split_out: dict[dt.date, float] = {}
    split_in: dict[dt.date, float] = {}
    split_text: dict[dt.date, str] = {}
    withdrawals: list[Deposit] = []
    currencies: set[str] = set()
    unknown: set[str] = set()

    for r in rows:
        s.names.add(r["Värdepapper"])
        if r.get("ISIN"):
            s.isins.add(r["ISIN"].upper())
        t, d = ttype(r), tdate(r)
        q = num(r.get("Antal")) or 0.0
        depa = r.get("Depå", "")
        text = r.get("Transaktionstext", "")
        dq = delta(t, q)
        if dq is None:
            unknown.add(r.get("Transaktionstyp", ""))
            dq = 0.0
        if depa not in hold:
            # Holding before the first row of this account. Dividend rows have
            # "Totalt antal" 0 but the holding in "Antal".
            after = num(r.get("Totalt antal"))
            if "UTDELNING" in t:
                hold[depa] = q
            elif after is not None and not is_ignored(t):
                hold[depa] = after - dq
            else:
                hold[depa] = 0.0
            s.opening += hold[depa]

        if "SPLIT" in t:
            side = split_in if "INLÄGG" in t else split_out
            side[d] = side.get(d, 0.0) + q
            split_text.setdefault(d, text)
        elif t in ("KÖPT", "SÅLT"):
            cur = r.get("Valuta (Belopp)", "")
            if cur:
                currencies.add(cur)
            amount = num(r.get("Belopp")) or 0.0
            ref = r.get("Notanummer") or r.get("Verifikationsnummer", "")
            if t == "KÖPT":
                s.events.append(Event(d, "Köp", q, -amount, note=f"Nordnet köpt {q:g} st", ref=ref))
            else:
                fee = num(r.get("Total Avgift")) or 0.0
                price = num(r.get("Kurs")) or 0.0
                s.events.append(Event(d, "Förs", q, proceeds=amount, ref=ref,
                                      nordnet_result=nordnet_result(r),
                                      note=f"Nordnet sålt {q:g} st à {price:g}, avgift {fee:g}"))
        elif "UTTAG" in t and "RESULTAT" in t:
            # Nordnet books a realised result: a disposal at market value, e.g. a move
            # into an ISK.
            price = num(r.get("Kurs")) or 0.0
            s.events.append(Event(d, "Förs", q, proceeds=q * price,
                                  ref=r.get("Notanummer") or r.get("Verifikationsnummer", ""),
                                  nordnet_result=nordnet_result(r),
                                  note=f"Nordnet {t.lower()} {q:g} st à {price:g} ('{text}'), "
                                       "avyttring till marknadsvärde"))
            warn(f"{s.name} {d}: {t} {q:g} st ('{text}') räknas som avyttring till "
                 f"marknadsvärde ({q:g} x {price:g} kr), t.ex. flytt till ISK - kontrollera.")
        elif "UTTAG" in t and "INLÖSEN" in t:
            s.events.append(Event(d, "Inl", q, proceeds=num(r.get("Belopp")) or 0.0,
                                  ref=r.get("Notanummer") or r.get("Verifikationsnummer", ""),
                                  note=f"Nordnet {t.lower()} {q:g} st ('{text}')"))
        elif "INLÄGG" in t and "TECKNING" in t:
            # Shares subscribed for in a new issue: a purchase at the subscription price.
            cost = num(r.get("Inköpsvärde"))
            if cost is None:
                cost = -(num(r.get("Belopp")) or 0.0) or q * (num(r.get("Kurs")) or 0.0)
            s.events.append(Event(d, "Köp", q, cost,
                                  ref=r.get("Notanummer") or r.get("Verifikationsnummer", ""),
                                  note=f"Nordnet teckning {q:g} st ('{text}'), {cost:g} kr"))
        elif "INLÄGG" in t:
            s.deposits.append(Deposit(d, q, text, depa, t, num(r.get("Inköpsvärde")),
                                      num(r.get("Kurs")) or 0.0,
                                      is_right(t, r["Värdepapper"], text)))
        elif is_out(t):
            withdrawals.append(Deposit(d, q, text, depa, t, None))

        hold[depa] += dq
        tot = num(r.get("Totalt antal"))
        if tot is not None and not is_ignored(t) and abs(tot - hold[depa]) > TOL:
            warn(f"{s.name} {d}: beräknat innehav {hold[depa]:g} i depå {depa} men "
                 f"'Totalt antal' {tot:g}.")
            hold[depa] = tot
        s.eod[d] = sum(hold.values())

    s.closing = sum(hold.values())
    s.closing_date = tdate(rows[-1])
    for u in sorted(unknown):
        warn(f"{s.name}: okänd transaktionstyp '{u}' ignorerad.")
    for c in sorted(currencies - {"SEK"}):
        warn(f"{s.name}: belopp i {c} - K4 och SKV 2198 ska vara i kronor, räkna om manuellt.")

    # Splits.
    for d in sorted(set(split_in) | set(split_out)):
        r_text = split_ratio(split_text.get(d, ""))
        r_qty = split_in.get(d, 0.0) / split_out[d] if split_out.get(d) else None
        if r_text and (r_qty is None or abs(r_qty / r_text - 1) < 0.05):
            ratio = r_text
        elif r_text and r_qty and abs(r_qty * r_text - 1) < 0.05:
            ratio = 1 / r_text
        elif r_qty:
            ratio = r_qty
            if r_text:
                warn(f"{s.name} {d}: splitkvot enligt texten ({split_text[d]}) stämmer inte med "
                     f"antalen - {ratio:g} används.")
        else:
            warn(f"{s.name} {d}: split utan känd kvot ('{split_text.get(d, '')}') - ignoreras.")
            continue
        label = f"{ratio:g}:1" if ratio >= 1 else f"1:{1 / ratio:g}"
        s.events.append(Event(d, "S", ratio=ratio, note=f"Split {label}"))

    # Moves: UTTAG VP in one account and INLÄGG VP of the same quantity in another within
    # TRANSFER_DAYS, or in the same account on the same day (Nordnet re-booking).
    for w in withdrawals:
        if conversion(w.kind):
            continue
        hit = next((dp for dp in s.deposits if not dp.internal and abs(dp.qty - w.qty) < TOL
                    and (dp.date == w.date if dp.depa == w.depa
                         else abs((dp.date - w.date).days) <= TRANSFER_DAYS)),
                   None)
        if hit:
            w.internal = hit.internal = True
            s.in_transit.append((min(w.date, hit.date), max(w.date, hit.date)))
    for w in withdrawals:
        if w.internal:
            continue
        carry = conversion(w.kind)
        what = ("utnyttjade vid teckning" if carry == "TECKNING" else
                "bytta mot andra värdepapper" if carry else
                "tas bort till genomsnittligt omkostnadsbelopp")
        s.events.append(Event(w.date, "Ut", w.qty, carry=carry,
                              note=f"Nordnet {w.kind.lower()} {w.qty:g} st ('{w.text}'), {what}"))
        if not carry:
            warn(f"{s.name} {w.date}: {w.kind} {w.qty:g} st ('{w.text}') - aktierna flyttades "
                 "från Nordnet och tas bort till genomsnittligt omkostnadsbelopp. Säljs de "
                 "någon annanstans gäller samma genomsnitt.")
    return s


# --------------------------------------------------------------------------- cost basis


def load_costs(path: str) -> list[CostRow]:
    out = []
    base = os.path.basename(path)
    for i, r in enumerate(read_table(path), start=2):
        try:
            d = dt.date.fromisoformat(r.get("Datum", "").strip())
        except ValueError:
            warn(f"{base} rad {i}: ogiltigt datum '{r.get('Datum', '')}' - raden hoppas över.")
            continue
        q = num(r.get("Antal"))
        if not q:
            warn(f"{base} rad {i}: Antal saknas - raden hoppas över.")
            continue
        sec = r.get("Värdepapper", "").strip()
        if not sec:
            warn(f"{base} rad {i}: Värdepapper saknas - raden hoppas över.")
            continue
        kind = (r.get("Händelse") or "Köp").strip().capitalize()
        if kind not in COST_KINDS:
            warn(f"{base} rad {i}: okänd Händelse '{kind}', Övr används.")
            kind = "Övr"
        out.append(CostRow(sec, d, q, num(r.get("Omkostnadsbelopp")), kind, r.get("Kommentar", "")))
    return out


def deposit_events(s: Security, costs: list[CostRow], have_file: bool) -> list[Event]:
    """Turn a security's deposits (and its opening holding) into ledger events using the
    cost file, else Nordnet's inköpsvärde."""
    events: list[Event] = []
    start = s.start_date
    by_date: dict[dt.date, list[CostRow]] = {}
    for c in costs:
        by_date.setdefault(c.date, []).append(c)

    def from_rows(rows: list[CostRow], prefix: str) -> float:
        total = 0.0
        for c in rows:
            if c.cost is None:
                warn(f"{s.name} {c.date}: {c.qty:g} st i kostnadsfilen saknar omkostnadsbelopp "
                     "- 0 kr används.")
            note = prefix + (f", {c.note}" if c.note else "")
            events.append(Event(c.date, c.kind, c.qty, c.cost or 0.0, note=note))
            total += c.qty
        return total

    # Opening holding (shares in Nordnet before the export starts).
    before = sorted((c for c in costs if c.date < start), key=lambda c: c.date)
    got = from_rows(before, "Ingående innehav enligt kostnadsfilen")
    if (s.opening > TOL or got > TOL) and abs(got - s.opening) > TOL:
        missing = s.opening - got
        if missing > 0:
            events.append(Event(start - dt.timedelta(days=1), "Övr", missing, 0.0,
                                note="Ingående innehav utan känt omkostnadsbelopp"))
        warn(f"{s.name}: Nordnet-exporten börjar {start} med {s.opening:g} st men kostnadsfilen "
             f"har {got:g} st daterade före det - ange ingående innehav med omkostnadsbelopp "
             "(rader daterade före exportens början).")

    # Deposits, grouped per date.
    deposited: dict[dt.date, list[Deposit]] = {}
    for dp in s.deposits:
        if not dp.internal:
            deposited.setdefault(dp.date, []).append(dp)
    missing_dates = []
    for d, items in sorted(deposited.items()):
        qty = sum(x.qty for x in items)
        texts = ", ".join(sorted({x.text for x in items if x.text}))
        prefix = f"Nordnet inlägg {qty:g} st" + (f" ('{texts}')" if texts else "")
        rows = by_date.get(d, [])
        got = from_rows(rows, prefix)
        rest = qty - got
        isk_missing = False
        if rest > TOL and not rows:
            # No cost file rows: rights get 0 kr, otherwise Nordnet's inköpsvärde, or the
            # market value for shares taken out of an ISK.
            for x in items:
                if x.right:
                    events.append(Event(d, "Övr", x.qty, 0.0,
                                        note=f"{prefix}, tilldelade rätter, omkostnadsbelopp 0 kr"))
                elif x.nordnet_cost is not None:
                    events.append(Event(d, "Övr", x.qty, x.nordnet_cost,
                                        note=f"{prefix}, inköpsvärde {x.nordnet_cost:g} enligt Nordnet"))
                    warn(f"{s.name} {d}: {x.kind} {x.qty:g} st medtagen med inköpsvärde "
                         f"{x.nordnet_cost:g} kr enligt Nordnet - kontrollera.")
                elif from_isk(x) and x.price:
                    events.append(Event(d, "Övr", x.qty, x.qty * x.price,
                                        note=f"{prefix}, marknadsvärde {x.qty:g} x {x.price:g}"))
                    warn(f"{s.name} {d}: {x.qty:g} st från ISK medtagna till marknadsvärde "
                         f"{x.qty:g} x {x.price:g} kr - kontrollera.")
                else:
                    if from_isk(x):
                        isk_missing = True
                        warn(f"{s.name} {d}: {x.qty:g} st flyttade från ISK ('{x.text}') - "
                             "omkostnadsbeloppet är marknadsvärdet på överföringsdagen, ange det "
                             "i kostnadsfilen.")
                    continue
                rest -= x.qty
        if rest > TOL:
            events.append(Event(d, "Övr", rest, 0.0, note=prefix + ", omkostnadsbelopp saknas (0 kr)"))
            if rows:
                warn(f"{s.name} {d}: inlägg {qty:g} st men kostnadsfilen har {got:g} st - "
                     f"{rest:g} st tas med till 0 kr.")
            elif not isk_missing:
                missing_dates.append(d)
        elif got - qty > TOL:
            warn(f"{s.name} {d}: kostnadsfilen har {got:g} st men inlägget är {qty:g} st.")
    if missing_dates:
        if not have_file:
            warn(f"{s.name}: ingen kostnadsfil ({COST_PATTERN}) - {len(missing_dates)} inlägg tas "
                 f"med till 0 kr. Fyll i {TEMPLATE} och spara den som Omkostnadsbelopp.csv.")
        else:
            warn(f"{s.name}: {len(missing_dates)} inlägg saknas i kostnadsfilen och tas med till "
                 "0 kr: " + ", ".join(str(d) for d in missing_dates))

    for d, rows in sorted(by_date.items()):
        if d >= start and d not in deposited:
            warn(f"Kostnadsfilen {s.name} {d}: {sum(c.qty for c in rows):g} st matchar inget "
                 "inlägg i Nordnet - ignoreras.")
    return events


def from_isk(dp: Deposit) -> bool:
    return bool(re.search(r"\bISK\b", dp.text.upper()))


def write_template(path: str, securities: list[Security]) -> None:
    with open(path, "w", encoding="utf-8-sig", newline="") as f:
        w = csv.writer(f, delimiter=";")
        w.writerow(["Värdepapper", "Datum", "Antal", "Omkostnadsbelopp", "Händelse", "Kommentar"])
        for s in securities:
            if s.opening > TOL:
                w.writerow([s.name, s.start_date - dt.timedelta(days=1), fmt_plain(s.opening), "",
                            "Köp", "Ingående innehav före Nordnet-exporten"])
            for dp in sorted((x for x in s.deposits if not x.internal and not x.right),
                             key=lambda x: x.date):
                cost = "" if dp.nordnet_cost is None else fmt_plain(dp.nordnet_cost)
                note = "Marknadsvärde på överföringsdagen (från ISK)" if from_isk(dp) else ""
                w.writerow([s.name, dp.date, fmt_plain(dp.qty), cost,
                            "Köp" if dp.nordnet_cost is None else "Övr", note])


# --------------------------------------------------------------------------- calculation


def compute(events: list[Event], name: str):
    rows, sales = [], []
    total_qty = 0.0
    total_cost = 0.0
    for e in sorted(events, key=lambda e: (e.date, e.order)):
        kind, note = e.kind, e.note
        if kind == "S":
            delta_q, change = total_qty * (e.ratio - 1), 0.0
        elif kind in ("Förs", "Ut", "Inl"):
            if e.qty > total_qty + TOL:
                warn(f"{name} {e.date}: {'uttag' if kind == 'Ut' else 'försäljning'} av "
                     f"{e.qty:g} st men endast {total_qty:g} st i beräkningen - historiken är "
                     "ofullständig.")
            avg_before = total_cost / total_qty if total_qty else 0.0
            delta_q, change = -e.qty, -e.qty * avg_before + 0.0  # avoid -0.0
            if kind == "Ut" and e.carry == "TECKNING" and abs(change) >= 0.005:
                warn(f"{name} {e.date}: omkostnadsbelopp {-change:.2f} kr för de utnyttjade "
                     "rätterna ska läggas till de tecknade aktiernas - ange det i kostnadsfilen.")
            if kind == "Inl":
                if abs(e.proceeds) < 0.005 and abs(change) < 0.005:
                    kind, note = "Ut", note + ", utan ersättning och utan omkostnadsbelopp"
                else:
                    kind = "Förs"
                    if abs(e.proceeds) < 0.005:
                        warn(f"{name} {e.date}: inlösen/förfall av {e.qty:g} st utan belopp "
                             "redovisas som försäljning för 0 kr - kontrollera om likvid "
                             "bokats på en egen rad.")
            if kind == "Förs":
                sales.append({"Datum": e.date.isoformat(), "Antal": e.qty, "Beteckning": name,
                              "Försäljningspris": e.proceeds, "Omkostnadsbelopp": 0.0 - change,
                              "Resultat": e.proceeds + change, "Nota": e.ref})
                if e.nordnet_result is not None and \
                        abs(e.nordnet_result - sales[-1]["Resultat"]) > 1:
                    warn(f"{name} {e.date}: resultat {sales[-1]['Resultat']:.2f} kr men "
                         f"{e.nordnet_result:.2f} kr enligt Nordnet (Nordnet räknar med sitt "
                         "eget anskaffningsvärde) - kontrollera omkostnadsbeloppet.")
        else:
            delta_q, change = e.qty, e.cost
        total_qty += delta_q
        total_cost += change
        if abs(total_qty) < TOL:
            total_qty, total_cost = 0.0, 0.0
        avg = total_cost / total_qty if total_qty else 0.0
        rows.append({
            "Beteckning": name,
            "Datum": e.date.isoformat(),
            "Händelse": "Övr" if kind == "Ut" else kind,
            "Antal +/-": delta_q,
            "Totalt antal": total_qty,
            "Inköpspris (+) / utnyttjat omk.belopp (-), kr": change,
            "Totalt outnyttjat omk.belopp, kr": total_cost,
            "Genomsnittligt omk.belopp, kr": avg,
            "Kommentar": note,
        })
    return rows, sales


# --------------------------------------------------------------------------- holdings log

LOG_LABEL = {"S": "Split", "Köp": "Köp", "Förm": "Förm", "Övr": "Övr", "Förs": "Försäljning"}


def build_log(s: Security, rows: list[dict], sales: list[dict]) -> list[dict]:
    """One row per SKV 2198 event with K4 values on sales and the holding after it."""
    sale_iter = iter(sales)
    log = []
    eod: dict[dt.date, float] = {}
    for r in rows:
        sale = next(sale_iter) if r["Händelse"] == "Förs" else None
        res = sale["Resultat"] if sale else None
        eod[dt.date.fromisoformat(r["Datum"])] = r["Totalt antal"]
        log.append({
            "Inkomstår": r["Datum"][:4], "Datum": r["Datum"], "Händelse": LOG_LABEL[r["Händelse"]],
            "Antal": r["Antal +/-"], "Beteckning": r["Beteckning"],
            "Försäljningspris": sale["Försäljningspris"] if sale else "",
            "Omkostnadsbelopp": sale["Omkostnadsbelopp"] if sale else "",
            "Vinst": max(res, 0.0) if sale else "", "Förlust": max(-res, 0.0) if sale else "",
            "Nota": sale["Nota"] if sale else "", "Kommentar": r["Kommentar"],
            "Kvar": r["Totalt antal"],
        })
    # Cross-check against Nordnet's "Totalt antal" (first mismatch, outside moves between
    # own accounts).
    for d, h in sorted(s.eod.items()):
        if any(a <= d < b for a, b in s.in_transit):
            continue
        if d in eod and abs(eod[d] - h) > TOL:
            warn(f"{s.name} {d}: beräknat innehav {eod[d]:g} st, enligt Nordnet {h:g} st "
                 "(första avvikelsen).")
            break
    return log


# --------------------------------------------------------------------------- output


def fmt_sv(v, decimals: int) -> str:
    return f"{v:,.{decimals}f}".replace(",", " ").replace(".", ",")


def fmt_qty(v: float) -> str:
    """Whole shares without decimals, fractions (e.g. fund units) with 4."""
    return fmt_sv(v, 0 if abs(v - round(v)) < 1e-9 else 4)


def fmt_plain(v: float) -> str:
    return f"{v:g}".replace(".", ",")


def write_csv(path, rows, decimals: dict[str, int], swedish: bool) -> None:
    with open(path, "w", encoding="utf-8-sig", newline="") as f:
        w = csv.DictWriter(f, fieldnames=list(rows[0].keys()), delimiter=";" if swedish else ",")
        w.writeheader()
        for r in rows:
            out = {}
            for k, v in r.items():
                if k in decimals and isinstance(v, (int, float)):
                    out[k] = fmt_sv(v, decimals[k]).replace(" ", "") if swedish \
                        else f"{v:.{decimals[k]}f}"
                else:
                    out[k] = v
            w.writerow(out)


def print_table(headers, data, left=()) -> None:
    widths = [max(len(h), *(len(d[i]) for d in data)) if data else len(h)
              for i, h in enumerate(headers)]
    line = lambda cells: "  ".join(c.ljust(w) if i in left else c.rjust(w)
                                   for i, (c, w) in enumerate(zip(cells, widths)))
    print(line(headers))
    print("  ".join("-" * w for w in widths))
    for d in data:
        print(line(d))


SKV_DEC = {"Antal +/-": 5, "Totalt antal": 5, "Inköpspris (+) / utnyttjat omk.belopp (-), kr": 2,
           "Totalt outnyttjat omk.belopp, kr": 2, "Genomsnittligt omk.belopp, kr": 2}
K4_DEC = {"Antal": 5, "Försäljningspris": 0, "Omkostnadsbelopp": 0, "Vinst": 0, "Förlust": 0}
LOG_DEC = {"Antal": 5, "Försäljningspris": 0, "Omkostnadsbelopp": 0, "Vinst": 0, "Förlust": 0,
           "Kvar": 5}


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0],
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("directory", nargs="?", default=".", help="folder with the exports")
    ap.add_argument("-o", "--output", default="skv2198.csv", help="SKV 2198 CSV (%(default)s)")
    ap.add_argument("--k4", default="k4.csv", help="K4 sales CSV (%(default)s)")
    ap.add_argument("--k4-holdings", default="k4_innehav.csv",
                    help="holdings log CSV, every event with holding after it (%(default)s)")
    ap.add_argument("--year", type=int, help="show K4 sales only for this income year")
    ap.add_argument("--security", action="append", metavar="NAMN|ISIN",
                    help="only this security (repeatable); default: all in the export")
    ap.add_argument("--depa", action="append", metavar="NR",
                    help="only this Nordnet account (Depå, repeatable); default: all. "
                         "Leave out ISK/KF accounts, they are not declared on K4")
    ap.add_argument("--nordnet", nargs="*", metavar="CSV",
                    help="Nordnet export file(s); default: Nordnet*.csv in the folder")
    ap.add_argument("--costs", metavar="CSV",
                    help=f"cost basis file for deposits; default: {COST_PATTERN} in the folder")
    ap.add_argument("--decimal-point", action="store_true",
                    help="write CSV with '.' decimals and ',' separator")
    args = ap.parse_args()

    nn_paths = args.nordnet if args.nordnet is not None else find(args.directory, "Nordnet*.csv")
    if not nn_paths:
        ap.exit(1, f"Nordnet*.csv hittades inte i {args.directory}\n")
    securities, known = load_nordnet(nn_paths, args.depa, args.security)
    if not securities:
        ap.exit(1, "Inga värdepapperstransaktioner i Nordnet-filerna.\n")

    cost_path = args.costs or next(iter(find(args.directory, COST_PATTERN)), None)
    costs = load_costs(cost_path) if cost_path else []
    if not cost_path and any(not d.internal for s in securities for d in s.deposits):
        template = os.path.normpath(os.path.join(args.directory, TEMPLATE))
        write_template(template, securities)
        print(f"Mall för omkostnadsbelopp sparad: {template}\n")
    used: set[int] = set()

    all_rows, all_sales, all_log, summary = [], [], [], []
    for s in securities:
        mine = [c for c in costs if s.matches(c.security)]
        used.update(id(c) for c in mine)
        events = s.events + deposit_events(s, mine, cost_path is not None)
        rows, sales = compute(events, s.name)
        if not rows:
            continue
        print(f"SKV 2198 - Genomsnittligt omkostnadsbelopp - {s.name}\n")
        print_table(
            ["Datum", "Händ.", "Antal +/-", "Totalt antal", "Inköpspris/utnyttjat",
             "Totalt omk.belopp", "Genomsn. omk.belopp", "Kommentar"],
            [[r["Datum"], r["Händelse"], fmt_sv(r["Antal +/-"], 5), fmt_sv(r["Totalt antal"], 5),
              fmt_sv(r["Inköpspris (+) / utnyttjat omk.belopp (-), kr"], 2),
              fmt_sv(r["Totalt outnyttjat omk.belopp, kr"], 2),
              fmt_sv(r["Genomsnittligt omk.belopp, kr"], 2), r["Kommentar"]] for r in rows],
            left={0, 1, 7})
        print()
        all_rows += rows
        all_sales += sales
        all_log += build_log(s, rows, sales)
        last = rows[-1]
        summary.append([s.name, fmt_sv(last["Totalt antal"], 5), fmt_sv(s.closing, 5),
                        fmt_sv(last["Genomsnittligt omk.belopp, kr"], 2)])
        if abs(s.closing - last["Totalt antal"]) > 0.01:
            warn(f"{s.name}: beräknat innehav {last['Totalt antal']:.5f} st skiljer sig från "
                 f"Nordnets {s.closing:.5f} st ({s.closing_date}).")
    for c in costs:
        if id(c) not in used and c.security.upper() not in known:
            warn(f"Kostnadsfilen: '{c.security}' {c.date} finns inte bland värdepappren i "
                 "Nordnet-exporten - ignoreras.")

    if all_rows:
        write_csv(args.output, all_rows, SKV_DEC, not args.decimal_point)
        print(f"SKV 2198-tabeller sparade: {args.output}")

    # K4
    shown = [s for s in all_sales if args.year is None or s["Datum"][:4] == str(args.year)]
    if shown:
        shown.sort(key=lambda s: (s["Datum"][:4], s["Beteckning"].upper(), s["Datum"]))
        k4_rows, data = [], []
        print("\nK4 avsnitt A - försäljningar")
        for y in sorted({s["Datum"][:4] for s in shown}):
            ys = [s for s in shown if s["Datum"][:4] == y]
            for s in ys:
                res = s["Resultat"]
                k4_rows.append({"Inkomstår": y, "Datum": s["Datum"], "Antal": s["Antal"],
                                "Beteckning": s["Beteckning"],
                                "Försäljningspris": s["Försäljningspris"],
                                "Omkostnadsbelopp": s["Omkostnadsbelopp"],
                                "Vinst": max(res, 0.0), "Förlust": max(-res, 0.0),
                                "Nota": s["Nota"]})
                data.append([y, s["Datum"], s["Beteckning"], fmt_qty(s["Antal"]),
                             fmt_sv(s["Försäljningspris"], 0), fmt_sv(s["Omkostnadsbelopp"], 0),
                             fmt_sv(max(res, 0), 0), fmt_sv(max(-res, 0), 0)])
            tp = sum(s["Försäljningspris"] for s in ys)
            to = sum(s["Omkostnadsbelopp"] for s in ys)
            gain = sum(max(s["Resultat"], 0.0) for s in ys)
            loss = sum(max(-s["Resultat"], 0.0) for s in ys)
            k4_rows.append({"Inkomstår": y, "Datum": "SUMMA", "Antal": "", "Beteckning": "",
                            "Försäljningspris": tp, "Omkostnadsbelopp": to, "Vinst": gain,
                            "Förlust": loss, "Nota": ""})
            data.append([y, "SUMMA K4", "", "", fmt_sv(tp, 0), fmt_sv(to, 0),
                         fmt_sv(gain, 0), fmt_sv(loss, 0)])
            data.append([""] * 8)
        print_table(["År", "Datum", "Värdepapper", "Antal", "Försäljningspris", "Omkostnadsbelopp",
                     "Vinst", "Förlust"], data[:-1], left={0, 1, 2})
        write_csv(args.k4, k4_rows, K4_DEC, not args.decimal_point)
        print(f"\nK4-underlag sparat: {args.k4}  (avrunda till hela kronor på blanketten)")

    # Holdings log
    if args.year is not None:
        all_log = [r for r in all_log if r["Inkomstår"] == str(args.year)]
    if all_log:
        write_csv(args.k4_holdings, all_log, LOG_DEC, not args.decimal_point)
        print(f"Innehavslogg (alla händelser, innehav efter varje) sparad: {args.k4_holdings}")

    print("\nSammanfattning")
    print_table(["Värdepapper", "Innehav beräknat", "Innehav Nordnet", "Genomsn. omk.belopp"],
                summary, left={0})

    if warnings:
        print("\nVarningar / kontrollera:")
        for m in warnings:
            print("  * " + m)


if __name__ == "__main__":
    main()
