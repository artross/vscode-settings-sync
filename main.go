package main

import (
	"archive/zip"
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// Задаем значение по умолчанию
const (
	DEFAULT_PORT = "8080"
)

// getVSCodePath возвращает путь к директории User настроек VS Code
func getVSCodePath() (string, error) {
	var basePath string

	switch runtime.GOOS {
	case "windows":
		basePath = os.Getenv("APPDATA")
		if basePath == "" {
			return "", fmt.Errorf("не удалось получить переменную APPDATA")
		}
		return filepath.Join(basePath, "Code", "User"), nil
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "Application Support", "Code", "User"), nil
	case "linux":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".config", "Code", "User"), nil
	default:
		return "", fmt.Errorf("неподдерживаемая ОС: %s", runtime.GOOS)
	}
}

// getLocalIP ищет реальный локальный IPv4 адрес (LAN), отсекая VPN, Docker и отключенные интерфейсы.
func getLocalIP() string {
	interfaces, err := net.Interfaces()
	if err != nil {
		return ""
	}

	// Черный список имен для виртуальных интерфейсов
	excludedPrefixes := []string{"docker", "br-", "veth", "virbr", "vboxnet"}

	for _, iface := range interfaces {
		// 1. Пропускаем отключенные интерфейсы, локалхост, VPN и PPPoE
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagPointToPoint != 0 {
			continue
		}

		// 2. Фильтруем по имени (Docker, VirtualBox и т.д.)
		// Дополнительная проверка на случай, если виртуалка не отмечена как P2P
		skip := false
		for _, prefix := range excludedPrefixes {
			if strings.HasPrefix(iface.Name, prefix) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}

		// Если интерфейс подошел, берем его адрес
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok {
				if ipnet.IP.To4() != nil {
					return ipnet.IP.String()
				}
			}
		}
	}

	return ""
}

// zipSource архивирует папку source в байтовый буфер
func zipSource(source string) (*bytes.Buffer, error) {
	buf := new(bytes.Buffer)
	w := zip.NewWriter(buf)

	walker := func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}

		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()

		// Создаем путь внутри архива относительно папки source
		f, err := w.Create(path[len(source):])
		if err != nil {
			return err
		}

		_, err = io.Copy(f, file)
		return err
	}

	err := filepath.Walk(source, walker)
	if err != nil {
		return nil, err
	}

	err = w.Close()
	if err != nil {
		return nil, err
	}

	return buf, nil
}

// unzipDest распаковывает архив из reader в папку dest
func unzipDest(reader io.Reader, dest string) error {
	os.MkdirAll(dest, 0755)

	buf := new(bytes.Buffer)
	buf.ReadFrom(reader)

	r, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		return err
	}

	for _, f := range r.File {
		fpath := filepath.Join(dest, f.Name)

		// Защита от ZipSlip
		if !strings.HasPrefix(fpath, filepath.Clean(dest)+string(os.PathSeparator)) {
			return fmt.Errorf("недопустимый путь файла: %s", fpath)
		}

		if f.FileInfo().IsDir() {
			os.MkdirAll(fpath, f.Mode())
			continue
		}

		parentDir := filepath.Dir(fpath)
		if err := os.MkdirAll(parentDir, 0755); err != nil {
			return err
		}

		outFile, err := os.OpenFile(fpath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
		if err != nil {
			return err
		}

		rc, err := f.Open()
		if err != nil {
			outFile.Close()
			return err
		}

		_, err = io.Copy(outFile, rc)
		outFile.Close()
		rc.Close()

		if err != nil {
			return err
		}
	}
	return nil
}

func backupDir(path string) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}

	timestamp := time.Now().Format("20060102-150405")
	dest := path + "_backup_" + timestamp

	fmt.Printf("Создание бэкапа текущих настроек в: %s\n", dest)
	return os.Rename(path, dest)
}

// --- ДЛЯ ВЕРСИИ 2.0 ---

// shouldSkip определяет лишние элементы, которые не нужно архивировать
func shouldSkip(path string) bool {
	var skipDirs = map[string]bool{
		"Cache":            true,
		"CachedData":       true,
		"Code Cache":       true,
		"languagepacks":    true, // обычно не нужно переносить
		"logs":             true,
		"workspaceStorage": true, // СОХРАНЯЕМ КОНТЕКСТ КЛИЕНТА
		"globalStorage":    true, // СОХРАНЯЕМ КОНТЕКСТ КЛИЕНТА
	}

	parts := strings.Split(path, string(filepath.Separator))
	for _, part := range parts {
		if skipDirs[part] {
			return true
		}
	}
	// Игнорируем сокеты и временные файлы БД
	if strings.HasSuffix(path, ".sock") || strings.HasSuffix(path, "-journal") {
		return true
	}
	return false
}

// addFolderToZip отправляет нужные файлы в поток архива
//   - folderPath — откуда берем (абсолютный путь на диске)
//   - zipPath — префикс внутри архива (например, "User" или "extensions")
//   - archive — наш запущенный зип-райтер
func addFolderToZip(folderPath string, zipPath string, archive *zip.Writer) error {
	return filepath.WalkDir(folderPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Вычисляем путь внутри архива
		relPath, _ := filepath.Rel(folderPath, path)
		// Соединяем с префиксом (например, "User/settings.json")
		entryName := filepath.Join(zipPath, relPath)

		// Наш фильтр из предыдущего шага
		if shouldSkip(relPath) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if d.IsDir() {
			return nil
		}

		// Создаем запись в архиве
		info, _ := d.Info()
		header, _ := zip.FileInfoHeader(info)
		header.Name = filepath.ToSlash(entryName) // ZIP всегда хочет "/" даже на Windows
		header.Method = zip.Deflate

		writer, err := archive.CreateHeader(header)
		if err != nil {
			return err
		}

		// Читаем файл и льем напрямую в архив (в сетевой поток)
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()

		_, err = io.Copy(writer, file)
		return err
	})
}

// --- СЕРВЕРНАЯ ЧАСТЬ ---

func runServer(port string) {
	localIP := getLocalIP()
	if localIP == "" {
		fmt.Printf("Ошибка при старте сервера: не определен ip-адрес для подключения")
		return
	}

	displayAddr := fmt.Sprintf("%s:%s", localIP, port)

	fmt.Println("========================================")
	fmt.Printf("✅ Сервер успешно запущен!\n")
	fmt.Printf("На клиенте используйте команду:\n")
	fmt.Printf("> vscode-settings-sync client %s\n", displayAddr)
	fmt.Println("========================================")
	fmt.Println("Ожидание подключений...")

	// Настраиваем HTTP-обработчик
	http.HandleFunc("/sync", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Только GET запросы", http.StatusMethodNotAllowed)
			return
		}

		fmt.Println("Запрос на синхронизацию получен. Подготовка архива...")

		// 1. Создаем архив, который пишет прямо в HTTP ответ,
		// предварительно задав ему правильные заголовки
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", "attachment; filename=vscode_settings.zip")
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		archive := zip.NewWriter(w)

		// ВАЖНО: сначала закрываем архив (записывается центральный каталог ZIP),
		// а потом обработчик завершает HTTP-сессию.
		defer archive.Close()

		// 2. Добавляем папки по очереди
		// Конфиги полетят в папку "User" внутри архива
		userDir := filepath.Join(os.Getenv("APPDATA"), "Code", "User")
		addFolderToZip(userDir, "User", archive)

		// Плагины полетят в папку "extensions" внутри архива
		extDir := filepath.Join(os.Getenv("USERPROFILE"), ".vscode", "extensions")
		addFolderToZip(extDir, "extensions", archive)

		fmt.Println("Архив передан.")
	})

	// Создаем сервер и запускаем в отдельной горутине
	srv := &http.Server{Addr: ":" + port}

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Printf("Ошибка сервера: %v\n", err)
		}
	}()

	// Ожидаем сигнал завершения (Ctrl+C)
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit // Программа "замрет" здесь, пока не нажмешь Ctrl+C

	fmt.Println("\n🛑 Получен сигнал остановки сервера. Ждем завершения текущих загрузок...")

	//  Даем серверу 5 секунд на закрытие всех текущих коннектов
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		fmt.Printf("Ошибка при закрытии сервера: %v\n", err)
	}

	fmt.Println("✅ Сервер успешно остановлен.")

}

// --- КЛИЕНТСКАЯ ЧАСТЬ ---

func runClient(serverIP string, port string) {
	vscodePath, err := getVSCodePath()
	if err != nil {
		fmt.Printf("Ошибка поиска папки VS Code: %v\n", err)
		return
	}

	url := fmt.Sprintf("http://%s:%s/sync", serverIP, port)
	fmt.Printf("Подключение к серверу: %s\n", url)

	resp, err := http.Get(url)
	if err != nil {
		fmt.Printf("Ошибка подключения к серверу: %v\n", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Printf("Сервер вернул ошибку: %s\n", resp.Status)
		return
	}

	// СОЗДАНИЕ БЭКАПА
	// Если бэкап не удается - ОСТАНАВЛИВАЕМ выполнение.
	// Мы не хотим перезаписывать настройки без страховки.
	if err := backupDir(vscodePath); err != nil {
		fmt.Printf("❌ ОШИБКА БЭКАПА: %v\n", err)
		fmt.Println("⛔  ВАЖНО: Операция синхронизации ОТМЕНЕНА для безопасности.")
		fmt.Println("Возможно, VS Code или другой процесс заблокировали папку.")
		fmt.Println("1. Закройте VS Code.")
		fmt.Println("2. Проверьте диспетчер задач на наличие процессов Code.exe.")
		fmt.Println("3. Попробуйте снова.")
		return
	}

	fmt.Println("✅ Бэкап создан успешно. Распаковка новых настроек...")
	if err := unzipDest(resp.Body, vscodePath); err != nil {
		fmt.Printf("Ошибка распаковки: %v\n", err)
		return
	}

	fmt.Println("🎉 Синхронизация успешно завершена! Перезапустите VS Code.")
}

// --- MAIN ---

func main() {
	// Определяем флаг для порта
	// flag.String возвращает *string (указатель).
	portPtr := flag.String("port", DEFAULT_PORT, "Порт для работы сервера/клиента")
	flag.Parse() // Парсим флаги, которые пользователь передал при запуске

	// После flag.Parse оставшиеся аргументы лежат в os.Args
	// os.Args[0] - имя программы
	// os.Args[1] - первая команда (server/client), если есть.
	// os.Args[2] - вторая команда (IP), если есть.

	if len(os.Args) < 2 {
		fmt.Println("Использование:")
		fmt.Println("  Сервер: vscode-settings-sync [-port ПОРТ] server")
		fmt.Println("  Клиент: vscode-settings-sync [-port ПОРТ] client <IP-адрес-сервера>")
		fmt.Println("\nПримеры:")
		fmt.Println("  vscode-settings-sync server")
		fmt.Println("  vscode-settings-sync -port 9000 client 192.168.1.50")
		fmt.Println("\nПо умолчанию используется порт 8080.")
		return
	}

	command := os.Args[1]

	switch command {
	case "server":
		runServer(*portPtr)
	case "client":
		if len(os.Args) < 3 {
			fmt.Println("Ошибка: укажите IP адрес сервера.")
			fmt.Println("Пример: vscode-settings-sync client 192.168.1.50")
			return
		}
		ip := os.Args[2]
		runClient(ip, *portPtr)
	default:
		fmt.Println("Неизвестная команда. Используйте 'server' или 'client'.")
	}
}
