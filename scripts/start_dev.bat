@echo off
cd /d "%~dp0.."
echo Building Open Fabrica...
echo Regenerating embedded assets...
go run openfabrica/internal/assetgen || (
    echo.
    echo Asset generation failed ^(admin UI^)! Press any key to close.
    pause >nul
    exit /b 1
)
go run openfabrica/knowledge/encgen || (
    echo.
    echo Asset generation failed ^(knowledge^)! Press any key to close.
    pause >nul
    exit /b 1
)
go generate ./cmd/openfabrica
go build -o openfabrica.exe ./cmd/openfabrica || (
    echo.
    echo Build failed! Press any key to close.
    pause >nul
    exit /b 1
)
openfabrica.exe
echo.
echo Open Fabrica has stopped. Press any key to close.
pause >nul
