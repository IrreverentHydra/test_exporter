package explorer

import (
	"errors"
	"fmt"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"time"

	logrusRotate "github.com/LazarenkoA/LogrusRotate"
	"github.com/LazarenkoA/prometheus_1C_exporter/explorers/model"
	"github.com/prometheus/client_golang/prometheus"
)

type ExplorerCheckSheduleJob struct {
	BaseRACExplorer

	baseList   []map[string]string
	dataGetter func() (map[string]bool, error)
	mx         *sync.RWMutex
	one        sync.Once
}

func (exp *ExplorerCheckSheduleJob) mutex() *sync.RWMutex {
	exp.one.Do(func() {
		exp.mx = new(sync.RWMutex)
	})

	return exp.mx
}

func (exp *ExplorerCheckSheduleJob) Construct(s model.Isettings, cerror chan error) *ExplorerCheckSheduleJob {
	exp.logger = logrusRotate.StandardLogger().WithField("Name", exp.GetName())
	exp.logger.Debug("Создание объекта")

	exp.gauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: exp.GetName(),
			Help: "Состояние галки \"блокировка регламентных заданий\", если галка установлена значение будет 1 иначе 0 или метрика будет отсутствовать",
		},
		[]string{"base"},
	)

	// dataGetter - типа мок. Инициализируется из тестов
	if exp.dataGetter == nil {
		exp.dataGetter = exp.getData
	}

	exp.settings = s
	exp.cerror = cerror
	prometheus.MustRegister(exp.gauge)
	return exp
}

func (exp *ExplorerCheckSheduleJob) StartExplore() {
	delay := reflect.ValueOf(exp.settings.GetProperty(exp.GetName(), "timerNotyfy", 10)).Int()
	exp.logger.WithField("delay", delay).Debug("Start")

	timerNotyfy := time.Second * time.Duration(delay)
	exp.ticker = time.NewTicker(timerNotyfy)

	// Получаем список баз в кластере
	if err := exp.fillBaseList(); err != nil {
		// Если была ошибка это не так критично т.к. через час список повторно обновится. Ошибка может быть если RAS не доступен
		exp.logger.WithError(err).Warning("Не удалось получить список баз")
	}

FOR:
	for {
		exp.Lock()
		exp.logger.Trace("Lock")
		func() {
			defer func() {
				exp.Unlock()
				exp.logger.Trace("Unlock")
			}()

			exp.logger.Trace("Старт итерации таймера")

			if listCheck, err := exp.dataGetter(); err == nil {
				exp.gauge.Reset()
				for key, value := range listCheck {
					if value {
						exp.gauge.WithLabelValues(key).Set(1)
					} else {
						exp.gauge.WithLabelValues(key).Set(0)
					}
				}
			} else {
				exp.gauge.Reset()
				exp.logger.WithError(err).Error("Произошла ошибка")
			}
		}()

		select {
		case <-exp.ctx.Done():
			break FOR
		case <-exp.ticker.C:
		}
	}
}

func (exp *ExplorerCheckSheduleJob) getData() (data map[string]bool, err error) {
	exp.logger.Trace("Получение данных")
	defer exp.logger.Trace("Данные получены")

	data = make(map[string]bool)

	// проверяем блокировку рег. заданий по каждой базе
	// информация по базе получается довольно долго, особенно если в кластере много баз (например тестовый контур), поэтому делаем через пул воркеров
	type dbinfo struct {
		guid, name string
		value      bool
	}
	chanIn := make(chan *dbinfo, 5)
	chanOut := make(chan *dbinfo)
	wg := new(sync.WaitGroup)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			for db := range chanIn {
				if baseinfo, err := exp.getInfoBase(db.guid, db.name); err == nil {
					db.value = strings.ToLower(baseinfo["scheduled-jobs-deny"]) != "off"
					chanOut <- db
				} else {
					exp.logger.WithError(err).Error()
				}
			}
		}()
	}

	go func() {
		wg.Wait()
		close(chanOut)
	}()

	go func() {
		for _, item := range exp.baseList {
			exp.logger.Tracef("Запрашиваем информацию для базы %s", item["name"])
			chanIn <- &dbinfo{name: item["name"], guid: item["infobase"]}
		}
		close(chanIn)
	}()

	for db := range chanOut {
		data[db.name] = db.value
	}

	return data, nil
}

func (exp *ExplorerCheckSheduleJob) getInfoBase(baseGuid, basename string) (map[string]string, error) {
	login, pass := exp.settings.GetLogPass(basename)
	if login == "" {
		CForce <- struct{}{} // принудительно запрашиваем данные из REST
		return nil, fmt.Errorf("для базы %s не определен пользователь", basename)
	}

	var param []string
	if exp.settings.RAC_Host() != "" {
		param = append(param, strings.Join(appendParam([]string{exp.settings.RAC_Host()}, exp.settings.RAC_Port()), ":"))
	}

	param = append(param, "infobase")
	param = append(param, "info")
	param = exp.appendLogPass(param)

	param = append(param, fmt.Sprintf("--cluster=%v", exp.GetClusterID()))
	param = append(param, fmt.Sprintf("--infobase=%v", baseGuid))
	param = append(param, fmt.Sprintf("--infobase-user=%v", login))
	param = append(param, fmt.Sprintf("--infobase-pwd=%v", pass))

	exp.logger.WithField("param", param).Debugf("Получаем информацию для базы %q", basename)
	if result, err := exp.run(exec.Command(exp.settings.RAC_Path(), param...)); err != nil {
		exp.logger.WithError(err).Error()
		return map[string]string{}, err
	} else {
		baseInfo := []map[string]string{}
		exp.formatMultiResult(result, &baseInfo)
		if len(baseInfo) > 0 {
			return baseInfo[0], nil
		} else {
			return nil, errors.New(fmt.Sprintf("Не удалось получить информацию по базе %q", basename))
		}
	}
}

func (exp *ExplorerCheckSheduleJob) findBaseName(ref string) string {
	exp.mutex().RLock()
	defer exp.mutex().RUnlock()

	for _, b := range exp.baseList {
		if b["infobase"] == ref {
			return b["name"]
		}
	}
	return ""
}

func (exp *ExplorerCheckSheduleJob) fillBaseList() error {
	if len(exp.baseList) > 0 { // Список баз может быть уже заполнен, например при тетсировании
		return nil
	}

	run := func() error {
		exp.mutex().Lock()
		defer exp.mutex().Unlock()

		var param []string
		if exp.settings.RAC_Host() != "" {
			param = append(param, strings.Join(appendParam([]string{exp.settings.RAC_Host()}, exp.settings.RAC_Port()), ":"))
		}

		param = append(param, "infobase")
		param = append(param, "summary")
		param = append(param, "list")
		param = exp.appendLogPass(param)
		param = append(param, fmt.Sprintf("--cluster=%v", exp.GetClusterID()))

		if result, err := exp.run(exec.Command(exp.settings.RAC_Path(), param...)); err != nil {
			exp.logger.WithError(err).Error("Ошибка получения списка баз")
			return err
		} else {
			exp.formatMultiResult(result, &exp.baseList)
		}

		return nil
	}

	// редко, но все же список баз может быть изменен поэтому делаем обновление периодическим, что бы не приходилось перезапускать экспортер
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()

		for range t.C {
			run()
		}
	}()

	return run()
}

func (exp *ExplorerCheckSheduleJob) GetName() string {
	return "SheduleJob"
}
