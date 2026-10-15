package budget

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zweiundeins/lith/loadtest"
	"github.com/zweiundeins/lith/sqlite"
	"github.com/zweiundeins/lith/sse"
	"github.com/zweiundeins/lith/writer"
)

// boardFrames renders loadtest.Board's board of 200 cards n times, as the
// hub does, with one card changed before each frame after the first:
// about 139 KB of HTML each.
func boardFrames(t testing.TB, n int) [][]byte {
	t.Helper()
	return boardRows(t, 200, n)
}

// boardRows is boardFrames with a board of rows cards.
func boardRows(t testing.TB, rows, n int) [][]byte {
	t.Helper()
	ctx := context.Background()
	b := loadtest.Board{Rows: rows}
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "board.db"), sqlite.Options{Migrations: b.Migrations()})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	w, err := writer.New(db, writer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	go w.Run(ctx)
	defer w.Close()
	if err := w.Do(ctx, b.Seed()); err != nil {
		t.Fatal(err)
	}
	view := b.View(0)
	frames := make([][]byte, n)
	for i := range frames {
		if i > 0 {
			if err := w.Do(ctx, b.Command(0, int64(i))); err != nil {
				t.Fatal(err)
			}
		}
		err := db.View(ctx, func(tx *sql.Tx) error {
			f, err := view.Render(ctx, tx)
			frames[i], _ = f.([]byte)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return frames
}

// pageFrames is a session of one tab of a small business app: lists of
// entries, a modal form, a detail page and an
// overview, 2 to 37 KB of HTML each. A frame is one view's render. Most
// frames repeat a view the tab saw before with a change (a row added,
// edited or deleted, the modal opened again, a list sorted); some show a
// view the tab has not seen for a while. The same seed makes the same
// session.
func pageFrames(n int, seed uint64) [][]byte {
	p := newPage(seed)
	frames := [][]byte{p.overview()}
	for len(frames) < n {
		frames = append(frames, p.step()...)
	}
	return frames[:n]
}

type entry struct {
	id     string
	day    int // days before the session
	cat    int
	note   string
	km     int
	amount int // centimes
}

type car struct {
	id, name, plate, fuel string
	year, km              int
	entries               []entry
}

type page struct {
	rnd     *rand.Rand
	cars    []*car
	cur     int    // the car whose list is open
	order   string // the list's sort
	filter  int    // the category the list shows, -1 for all
	nextKey int
}

var categories = []struct{ key, label, desc string }{
	{"fuel", "Treibstoff", "Tanken"},
	{"service", "Service", "Service"},
	{"insurance", "Versicherung", "Versicherungsprämie"},
	{"tyres", "Reifen", "Reifenwechsel"},
	{"parking", "Parkieren", "Parkgebühr"},
	{"tax", "Steuern", "Motorfahrzeugsteuer"},
}

var notes = []string{
	"Shell Altstetten", "BP Wallisellen", "Migrol Oerlikon", "Coop Pronto Dietikon",
	"Avia Schlieren", "Agrola Bülach", "Socar Kloten", "Tamoil Regensdorf",
	"Garage Meier AG", "Parkhaus Urania", "Strassenverkehrsamt", "Reifen Center Volketswil",
}

func newPage(seed uint64) *page {
	p := &page{rnd: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)), filter: -1, order: "date"}
	models := []struct {
		name, fuel string
		entries    int
	}{
		{"VW Golf Variant 2.0 TDI", "Diesel", 70},
		{"Škoda Octavia Combi RS", "Benzin", 55},
		{"Tesla Model 3 Long Range", "Elektro", 40},
		{"Toyota Yaris Hybrid", "Hybrid", 24},
		{"Fiat 500e", "Elektro", 10},
		{"Piaggio Ape 50", "Benzin", 2},
	}
	for i, m := range models {
		c := &car{
			id:    fmt.Sprintf("c%d", i+1),
			name:  m.name,
			plate: fmt.Sprintf("ZH %d %03d", 100+p.rnd.IntN(800), p.rnd.IntN(1000)),
			fuel:  m.fuel,
			year:  2012 + p.rnd.IntN(13),
			km:    20000 + p.rnd.IntN(140000),
		}
		for range m.entries {
			c.entries = append(c.entries, p.newEntry(c))
		}
		p.cars = append(p.cars, c)
	}
	return p
}

func (p *page) newEntry(c *car) entry {
	p.nextKey++
	cat := 0
	if p.rnd.IntN(3) == 0 {
		cat = 1 + p.rnd.IntN(len(categories)-1)
	}
	c.km += 200 + p.rnd.IntN(700)
	return entry{
		id:     fmt.Sprintf("%06x", uint32(p.nextKey)*2654435761>>8),
		day:    p.rnd.IntN(900),
		cat:    cat,
		note:   notes[p.rnd.IntN(len(notes))],
		km:     c.km,
		amount: 2000 + p.rnd.IntN(18000),
	}
}

// step does what a person does next and returns the frames it sends.
func (p *page) step() [][]byte {
	c := p.cars[p.cur]
	switch r := p.rnd.IntN(100); {
	case r < 14: // open another car's list
		p.cur, p.filter, p.order = p.rnd.IntN(len(p.cars)), -1, "date"
		return [][]byte{p.list()}
	case r < 40: // add an entry: the modal, then the list with it
		modal := p.modal(c, nil)
		c.entries = append(c.entries, p.newEntry(c))
		if len(c.entries) > 72 {
			c.entries = c.entries[1:]
		}
		return [][]byte{modal, p.list()}
	case r < 60: // edit an entry
		i := p.rnd.IntN(len(c.entries))
		modal := p.modal(c, &c.entries[i])
		c.entries[i].amount += p.rnd.IntN(500) - 250
		c.entries[i].note = notes[p.rnd.IntN(len(notes))]
		return [][]byte{modal, p.list()}
	case r < 68: // delete an entry
		if len(c.entries) > 3 {
			i := p.rnd.IntN(len(c.entries))
			c.entries = slices.Delete(c.entries, i, i+1)
		}
		return [][]byte{p.list()}
	case r < 80: // sort or filter the list
		if p.rnd.IntN(2) == 0 {
			p.order = []string{"date", "amount", "km"}[p.rnd.IntN(3)]
		} else {
			p.filter = p.rnd.IntN(len(categories)+1) - 1
		}
		return [][]byte{p.list()}
	case r < 90:
		return [][]byte{p.detail(c)}
	default:
		return [][]byte{p.overview()}
	}
}

func chf(centimes int) string { return fmt.Sprintf("%d.%02d", centimes/100, centimes%100) }

func km(n int) string {
	if n < 1000 {
		return fmt.Sprint(n)
	}
	return fmt.Sprintf("%d'%03d", n/1000, n%1000)
}

var today = time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)

func (e entry) row(b *bytes.Buffer, c *car) {
	d := today.AddDate(0, 0, -e.day)
	cat := categories[e.cat]
	fmt.Fprintf(b, `<tr id="e-%s" class="row row--%s"><td><time datetime="%s">%s</time></td><td>%s <small>%s</small></td><td class="num">%s</td><td class="num">%s</td><td><span class="tag tag--%s">%s</span></td><td class="act"><button class="ib" aria-label="Bearbeiten" data-on:click="@get('/%s/e/%s/edit')">✎</button><button class="ib ib--del" aria-label="Löschen" data-on:click="@post('/%s/e/%s/del')">✕</button></td></tr>`+"\n",
		e.id, cat.key, d.Format("2006-01-02"), d.Format("02.01.06"), cat.desc, e.note, km(e.km), chf(e.amount), cat.key, cat.label, c.id, e.id, c.id, e.id)
}

func (p *page) list() []byte {
	c := p.cars[p.cur]
	rows := slices.Clone(c.entries)
	if p.filter >= 0 {
		rows = slices.DeleteFunc(rows, func(e entry) bool { return e.cat != p.filter })
	}
	switch p.order {
	case "amount":
		slices.SortStableFunc(rows, func(a, b entry) int { return b.amount - a.amount })
	case "km":
		slices.SortStableFunc(rows, func(a, b entry) int { return b.km - a.km })
	default:
		slices.SortStableFunc(rows, func(a, b entry) int { return a.day - b.day })
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, `<section id="main" class="entries" data-car="%s"><header class="entries__head"><h1>%s <small>%s</small></h1><div class="toolbar"><input type="search" class="search" placeholder="Einträge durchsuchen" data-bind:query data-on:input__debounce.300ms="@get('/%s/entries')"><div class="chips" role="group" aria-label="Kategorie">`, c.id, c.name, c.plate, c.id)
	for i, cat := range categories {
		on := ""
		if i == p.filter {
			on = ` aria-pressed="true"`
		}
		fmt.Fprintf(&b, `<button class="chip chip--%s"%s data-on:click="@post('/%s/filter?cat=%s')">%s</button>`, cat.key, on, c.id, cat.key, cat.label)
	}
	fmt.Fprintf(&b, `</div><button class="btn btn--primary" data-on:click="@get('/%s/e/new')">Neuer Eintrag</button></div></header>`+"\n", c.id)
	b.WriteString(`<table class="table"><thead><tr>`)
	for _, h := range []struct{ key, label string }{{"date", "Datum"}, {"desc", "Beschreibung"}, {"km", "Kilometer"}, {"amount", "Betrag (CHF)"}, {"cat", "Kategorie"}} {
		fmt.Fprintf(&b, `<th scope="col"><button class="sort" data-on:click="@post('/%s/sort?by=%s')">%s</button></th>`, c.id, h.key, h.label)
	}
	b.WriteString(`<th scope="col"><span class="sr-only">Aktionen</span></th></tr></thead><tbody>` + "\n")
	total := 0
	for _, e := range rows {
		e.row(&b, c)
		total += e.amount
	}
	fmt.Fprintf(&b, `</tbody><tfoot><tr><th scope="row" colspan="3">%d Einträge</th><td class="num">%s</td><td colspan="2"></td></tr></tfoot></table></section>`, len(rows), chf(total))
	return sse.PatchElements(b.Bytes())
}

func (p *page) modal(c *car, e *entry) []byte {
	title, action, val := "Neuer Eintrag", "/"+c.id+"/e", entry{day: 0, km: c.km}
	if e != nil {
		title, action, val = "Eintrag bearbeiten", "/"+c.id+"/e/"+e.id, *e
	}
	d := today.AddDate(0, 0, -val.day)
	var b bytes.Buffer
	fmt.Fprintf(&b, `<dialog id="modal" class="modal" open data-on:close="@post('/modal/close')"><form class="form" data-on:submit__prevent="@post('%s', {contentType: 'form'})"><header class="modal__head"><h2>%s</h2><p class="muted">%s · %s</p><button type="button" class="ib" aria-label="Schliessen" data-on:click="@post('/modal/close')">✕</button></header>`+"\n", action, title, c.name, c.plate)
	fmt.Fprintf(&b, `<div class="field"><label for="f-date">Datum</label><input id="f-date" name="date" type="date" value="%s" max="%s" required></div>`+"\n", d.Format("2006-01-02"), today.Format("2006-01-02"))
	b.WriteString(`<div class="field"><label for="f-cat">Kategorie</label><select id="f-cat" name="category" data-bind:cat>`)
	for i, cat := range categories {
		sel := ""
		if i == val.cat {
			sel = " selected"
		}
		fmt.Fprintf(&b, `<option value="%s"%s>%s</option>`, cat.key, sel, cat.label)
	}
	b.WriteString(`</select></div>` + "\n")
	fmt.Fprintf(&b, `<div class="field"><label for="f-amount">Betrag (CHF)</label><input id="f-amount" name="amount" inputmode="decimal" value="%s" required pattern="[0-9]+([.,][0-9]{1,2})?"><p class="hint">Mit Rappen, zum Beispiel 92.35</p></div>`+"\n", chf(val.amount))
	fmt.Fprintf(&b, `<div class="field"><label for="f-km">Kilometerstand</label><input id="f-km" name="km" inputmode="numeric" value="%d" min="0"><p class="hint">Letzter Stand: %s km</p></div>`+"\n", val.km, km(c.km))
	fmt.Fprintf(&b, `<div class="field"><label for="f-note">Notiz</label><input id="f-note" name="note" value="%s" list="f-notes" maxlength="120"><datalist id="f-notes">`, val.note)
	for _, n := range notes {
		fmt.Fprintf(&b, `<option value="%s">`, n)
	}
	b.WriteString(`</datalist></div>` + "\n")
	if c.fuel != "Elektro" {
		b.WriteString(`<div class="field field--row" data-show="$cat === 'fuel'"><label for="f-liters">Liter</label><input id="f-liters" name="liters" inputmode="decimal" data-bind:liters><label class="check"><input type="checkbox" name="full" checked data-bind:full> Vollgetankt</label><p class="hint" data-text="$liters &amp;&amp; $amount ? (Number($amount) / Number($liters)).toFixed(3) + ' CHF/l' : ''"></p></div>` + "\n")
	} else {
		b.WriteString(`<div class="field field--row" data-show="$cat === 'fuel'"><label for="f-kwh">kWh</label><input id="f-kwh" name="kwh" inputmode="decimal" data-bind:kwh><label class="check"><input type="checkbox" name="home" data-bind:home> Zuhause geladen</label><p class="hint" data-text="$kwh &amp;&amp; $amount ? (Number($amount) / Number($kwh)).toFixed(3) + ' CHF/kWh' : ''"></p></div>` + "\n")
	}
	b.WriteString(`<div class="field"><label for="f-receipt">Beleg</label><input id="f-receipt" name="receipt" type="file" accept="image/*,application/pdf" data-on:change="@post('/upload', {contentType: 'form'})"><p class="hint">PDF oder Foto, höchstens 10 MB. Der Betrag wird aus dem Beleg gelesen, wenn das Feld leer ist.</p></div>` + "\n")
	b.WriteString(`<div class="field"><label class="check"><input type="checkbox" name="split" data-bind:split> Kosten aufteilen</label><div class="split" data-show="$split"><label for="f-share">Anteil Geschäft (%)</label><input id="f-share" name="share" type="range" min="0" max="100" step="5" value="50" data-bind:share><output data-text="$share + ' %'"></output></div></div>` + "\n")
	fmt.Fprintf(&b, `<footer class="modal__foot"><button type="submit" class="btn btn--primary" data-indicator:saving data-attr:disabled="$saving">Speichern</button><button type="button" class="btn" data-on:click="@post('/modal/close')">Abbrechen</button></footer></form></dialog>`)
	return sse.PatchElements(b.Bytes())
}

func (p *page) detail(c *car) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, `<section id="main" class="detail" data-car="%s"><header class="detail__head"><h1>%s</h1><p class="muted">%s · Jahrgang %d · %s</p><nav class="tabs" role="tablist"><button role="tab" aria-selected="true">Übersicht</button><button role="tab" data-on:click="@get('/%s/entries')">Einträge</button><button role="tab" data-on:click="@get('/%s/service')">Service</button><button role="tab" data-on:click="@get('/%s/docs')">Dokumente</button></nav></header>`+"\n", c.id, c.name, c.plate, c.year, c.fuel, c.id, c.id, c.id)
	b.WriteString(`<dl class="specs">`)
	for _, s := range [][2]string{{"Kilometerstand", km(c.km) + " km"}, {"Treibstoff", c.fuel}, {"Jahrgang", fmt.Sprint(c.year)}, {"Kontrollschild", c.plate}, {"Nächster Service", today.AddDate(0, 3, 0).Format("02.01.2006")}, {"Nächste MFK", today.AddDate(1, 2, 0).Format("02.01.2006")}, {"Versicherung", "Haftpflicht, Teilkasko"}, {"Leasing", "keines"}} {
		fmt.Fprintf(&b, `<div class="spec"><dt>%s</dt><dd>%s</dd></div>`, s[0], s[1])
	}
	b.WriteString(`</dl>` + "\n" + `<div class="costs"><h2>Kosten nach Kategorie</h2>`)
	sums := make([]int, len(categories))
	total := 0
	for _, e := range c.entries {
		sums[e.cat] += e.amount
		total += e.amount
	}
	for i, cat := range categories {
		pct := 0
		if total > 0 {
			pct = sums[i] * 100 / total
		}
		fmt.Fprintf(&b, `<div class="bar bar--%s"><span class="bar__label">%s</span><span class="bar__track"><span class="bar__fill" style="width: %d%%"></span></span><span class="bar__value num">%s</span></div>`+"\n", cat.key, cat.label, pct, chf(sums[i]))
	}
	b.WriteString(`</div><table class="table table--months"><thead><tr><th scope="col">Monat</th><th scope="col">Kosten (CHF)</th><th scope="col">Kilometer</th><th scope="col">CHF/100 km</th></tr></thead><tbody>` + "\n")
	for m := range 12 {
		month := today.AddDate(0, -m, 0)
		cost, dist := 0, 0
		for _, e := range c.entries {
			if e.day/30 == m {
				cost += e.amount
				dist += 600
			}
		}
		per := "–"
		if dist > 0 {
			per = chf(cost * 100 / dist)
		}
		fmt.Fprintf(&b, `<tr><th scope="row">%s</th><td class="num">%s</td><td class="num">%s</td><td class="num">%s</td></tr>`+"\n", month.Format("01/2006"), chf(cost), km(dist), per)
	}
	b.WriteString(`</tbody></table><h2>Letzte Einträge</h2><table class="table"><tbody>` + "\n")
	recent := slices.Clone(c.entries)
	slices.SortStableFunc(recent, func(a, b entry) int { return a.day - b.day })
	for _, e := range recent[:min(10, len(recent))] {
		e.row(&b, c)
	}
	b.WriteString(`</tbody></table></section>`)
	return sse.PatchElements(b.Bytes())
}

func (p *page) overview() []byte {
	var b bytes.Buffer
	b.WriteString(`<section id="main" class="overview"><header class="overview__head"><h1>Meine Fahrzeuge</h1><button class="btn btn--primary" data-on:click="@get('/cars/new')">Fahrzeug erfassen</button></header><div class="cards">` + "\n")
	for _, c := range p.cars {
		total, fuel := 0, 0
		for _, e := range c.entries {
			total += e.amount
			if e.cat == 0 {
				fuel += e.amount
			}
		}
		last := "–"
		if len(c.entries) > 0 {
			last = today.AddDate(0, 0, -c.entries[len(c.entries)-1].day).Format("02.01.2006")
		}
		fmt.Fprintf(&b, `<article id="car-%s" class="card"><a class="card__link" href="/%s" data-on:click__prevent="@get('/%s')"><h2>%s</h2><p class="muted">%s · %s</p></a><dl class="card__stats"><div><dt>Kosten total</dt><dd class="num">CHF %s</dd></div><div><dt>davon Treibstoff</dt><dd class="num">CHF %s</dd></div><div><dt>Kilometerstand</dt><dd class="num">%s km</dd></div><div><dt>Letzter Eintrag</dt><dd>%s</dd></div></dl></article>`+"\n",
			c.id, c.id, c.id, c.name, c.plate, c.fuel, chf(total), chf(fuel), km(c.km), last)
	}
	b.WriteString(`</div></section>`)
	return sse.PatchElements(b.Bytes())
}

// sizes summarises frames: the smallest, the mean and the largest, in bytes.
func sizes(frames [][]byte) string {
	lo, hi, sum := len(frames[0]), 0, 0
	for _, f := range frames {
		lo, hi, sum = min(lo, len(f)), max(hi, len(f)), sum+len(f)
	}
	return fmt.Sprintf("%d frames, %d to %d bytes, %d on average", len(frames), lo, hi, sum/len(frames))
}

func TestWorkloads(t *testing.T) {
	page := pageFrames(150, 1)
	t.Log("page:", sizes(page))
	if !strings.Contains(string(page[1]), "data-on:click") {
		t.Fatal("no page frame")
	}
	if testing.Short() {
		return
	}
	board := boardFrames(t, 3)
	t.Log("board:", sizes(board))
}
