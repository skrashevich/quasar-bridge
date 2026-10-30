package bridge

import (
	"context"
	"testing"

	"quasarbridge/internal/llm"
)

func TestUnitConversionReachesYandex(t *testing.T) {
	cfg := DefaultRoutingConfig()
	cfg.UnknownToHome = true
	e := &Engine{
		Home: homeFunc(func(context.Context, string) (string, error) { return "дом", nil }),
		LLM: completionFunc(func(context.Context, []llm.Message, bool) (string, error) {
			t.Fatal("conversion reached the classifier")
			return "", nil
		}),
	}
	if err := e.SetRouting(cfg); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{
		"50 евро в рублях",
		"5 долларов в рублях",
		"5 долларов сша в рублях",
		"5 долларов США в рублях",
		"пять долларов сша в российских рублях",
		"5 долларов соединенных штатов в рублях",
		"5 usd в рублях",
		"5$ в рублях",
		"5 $ в рублях",
		"5 долл. в рублях",
		"5 евро в долларе",
		"5 фунтов стерлингов в рублях",
		"40 вон в долларах",
		"20 кг в граммах",
		"1 метр в сантиметрах",
		"Алиса, 50 евро в рублях",
		"сколько будет 20 кг в граммах",
		"1,5 литра в миллилитрах",
		"пятьдесят евро в рублях",
		"переведи 100 долларов в евро",
		"2 квадратных метра в квадратных сантиметрах",
		"60 км в час",
		"36.6 градуса в фаренгейтах",
		"сто двадцать пять кг в г",
	} {
		got := e.Decide(t.Context(), text)
		if got.Route != "yandex" || got.Rule != "unit_conversion" || got.Source != "rule" {
			t.Errorf("%q: %+v", text, got)
		}
	}
	for _, text := range []string{
		"5 евро в кошельке",
		"5 долларов сша в кошельке",
		"5 кг сша в граммах",
		"курс доллара",
		"20 кг картошки",
		"10 метров в длину",
		"что такое евро",
		"включи свет в зале",
		"поставь таймер на 5 минут",
		"напиши код перевода 20 кг в граммах",
		"Включи музыку",
	} {
		got := e.Decide(t.Context(), text)
		if got.Rule == "unit_conversion" {
			t.Errorf("false conversion: %q %+v", text, got)
		}
	}
	if got := e.Decide(t.Context(), "5 евро в кошельке"); got.Route != "home" || got.Source != "unknown" {
		t.Fatalf("non-conversion: %+v", got)
	}
	if got := e.Decide(t.Context(), "напиши код перевода 20 кг в граммах"); got.Route != "llm" || got.Rule != "complex_queries" {
		t.Fatalf("code request: %+v", got)
	}
	if got := e.Decide(t.Context(), "поставь таймер на 5 минут"); got.Route != "yandex" || got.Rule != "yandex_services" {
		t.Fatalf("timer: %+v", got)
	}
}
