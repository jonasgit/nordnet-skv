Ett verktyg för att översätta vad Nordnet redovisar till vad Skatteverket vill ha: SKV 2198
"Genomsnittligt omkostnadsbelopp" per värdepapper och K4 avsnitt A för försäljningar. Både
som Python och Go.

Ladda ner fil från Nordnet (Transaktioner och notor, hela historiken, alla depåer):
Nordnet*.csv

Alla värdepapper i exporten räknas var för sig. Rader med samma namn eller ISIN hör ihop,
så historiken följer med när ett ISIN byts (t.ex. vid split). Splitar läses ur Nordnets
SPLIT-rader. Flyttar mellan egna depåer (UTTAG VP + INLÄGG VP med samma antal inom en
vecka) räknas inte som förvärv eller avyttring.

Särskilda transaktioner:

- Tilldelade teckningsrätter/uniträtter (TILLDELNING INLÄGG) får omkostnadsbelopp 0 kr.
- Förfallna rätter (INLÖSEN UTTAG VP utan belopp och utan omkostnadsbelopp) tas bort utan
  K4-rad. Har de ett omkostnadsbelopp, t.ex. köpta rätter, redovisas de som försäljning för
  0 kr.
- Flytt till ISK (UTTAG VP RESULTAT) är en avyttring till marknadsvärde och kommer med på
  K4. Resultatet stäms av mot Nordnets kolumn Resultat.
- Flytt från ISK (INLÄGG med ISK i texten) får marknadsvärdet på överföringsdagen som
  omkostnadsbelopp. Nordnet anger det sällan, så ange det i kostnadsfilen.
- Nordnets ombokningar inom samma depå samma dag (t.ex. EM UTTAG + EM INLÄGG) påverkar inte
  beräkningen.

ISK och kapitalförsäkring deklareras inte på K4. Ta bara med vanliga depåer:

    python3 nordnet_skv.py --depa 1234567

Aktier som satts in i Nordnet (INLÄGG VP, t.ex. överföringar från en annan förvaltare) har
oftast inget omkostnadsbelopp i Nordnets export. Ange det i en egen fil:
Omkostnadsbelopp.csv

    Värdepapper;Datum;Antal;Omkostnadsbelopp;Händelse;Kommentar
    VOLVO B;2024-05-27;262;52400;Köp;
    SE0000115446;2024-10-24;150;30000;Köp;köpta aktier
    SE0000115446;2024-10-24;50;12500;Förm;förmånsbeskattade aktier

- Värdepapper är namnet eller ISIN som i Nordnet-exporten.
- Datum (ÅÅÅÅ-MM-DD) och Antal ska stämma med inlägget i Nordnet. Flera rader samma datum
  summeras, t.ex. om ett inlägg innehåller både köpta och förmånsbeskattade aktier.
- Omkostnadsbelopp är totalen i kr för raden. Händelse är Köp, Förm eller Övr (Köp om tom).
- Rader daterade före värdepapprets första transaktion i exporten räknas som ingående
  innehav.

Saknas filen skriver programmet en mall, mall_omkostnadsbelopp.csv, med alla inlägg
ifyllda (med Nordnets inköpsvärde där det finns). Fyll i omkostnadsbeloppen och spara den
som Omkostnadsbelopp.csv. Inlägg utan omkostnadsbelopp räknas med Nordnets inköpsvärde om
det finns, annars 0 kr, och ger en varning.

Kör programmet i samma katalog som filerna ligger:

    python3 nordnet_skv.py [--security NAMN|ISIN] [--depa NR] [--year ÅÅÅÅ]
    nordnetskv/nordnetskv  [-security NAMN|ISIN]  [-depa NR]  [-year ÅÅÅÅ]
                           (bygg med nordnetskv/build.sh)

Resultat: skv2198.csv, k4.csv och k4_innehav.csv. Belopp i annan valuta än SEK ger en
varning och måste räknas om till kronor.
